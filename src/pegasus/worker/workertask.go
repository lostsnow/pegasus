package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"pegasus/log"
	"pegasus/server"
	"pegasus/task"
	"pegasus/taskreg"
	"pegasus/uri"
	"pegasus/util"
	"sync"
	"time"
)

var tskctx = &TaskCtx{}

const (
	BUF_TASKLET_CNT = 8
	// TODO test purpose
	//RUNNING_EXECUTOR_CNT = 4
	RUNNING_EXECUTOR_CNT = 2
	TASKLET_MAX_RETRY    = 3
)

type TaskCtx struct {
	tsk            task.Task
	wgFinish       sync.WaitGroup
	taskletCtxList []task.TaskletCtx
	todoTasklets   chan task.Tasklet
	doneTasklets   chan task.Tasklet
	// Following fields under mutex protection
	mutex    sync.Mutex
	err      error
	free     bool
	total    int
	done     int
	finished bool
	startTs  time.Time
	endTs    time.Time
}

func (ctx *TaskCtx) kickoff() {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	ctx.startTs = time.Now()
	ctx.err = nil
	ctx.total = 0
	ctx.done = 0
	ctx.finished = false
	ctx.endTs = time.Time{}
}

func (ctx *TaskCtx) finish() {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	ctx.finished = true
	ctx.endTs = time.Now()
}

func (ctx *TaskCtx) init() {
	taskletCnt := ctx.tsk.GetTaskletCnt()
	log.Info("Task %q tasklet count %d", ctx.tsk.GetTaskId(), taskletCnt)
	ctx.todoTasklets = make(chan task.Tasklet, BUF_TASKLET_CNT)
	ctx.doneTasklets = make(chan task.Tasklet, taskletCnt)
	ctx.taskletCtxList = make([]task.TaskletCtx, 0)
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	ctx.err = nil
	ctx.total = taskletCnt
}

func (ctx *TaskCtx) aborted() bool {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	if ctx.err == nil {
		return false
	} else {
		return true
	}
}

func (ctx *TaskCtx) setErr(err error) {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	ctx.err = err
}

func (ctx *TaskCtx) checkAndUnsetFree(tsk task.Task) error {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	if !ctx.free {
		return fmt.Errorf("Worker busy with task %q", ctx.tsk.GetKind())
	}
	ctx.free = false
	ctx.tsk = tsk
	return nil
}

func (ctx *TaskCtx) setFree() {
	log.Info("Set worker free")
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	ctx.free = true
	ctx.tsk = nil
}

func (ctx *TaskCtx) appendDoneTasklet(tasklet task.Tasklet) {
	ctx.doneTasklets <- tasklet
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	ctx.done++
}

func (ctx *TaskCtx) getTaskStatus() *task.TaskStatus {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	if ctx.free {
		return nil
	}
	return &task.TaskStatus{
		Tid:      ctx.tsk.GetTaskId(),
		Desc:     ctx.tsk.GetDesc(),
		StartTs:  ctx.startTs,
		Finished: ctx.finished,
		Total:    ctx.total,
		Done:     ctx.done,
	}
}

func getExecutorCnt() int {
	return RUNNING_EXECUTOR_CNT
}

func prepareExecutors(ctx *TaskCtx, tsk task.Task) {
	cnt := getExecutorCnt()
	for i := 0; i < cnt; i++ {
		c := tsk.NewTaskletCtx()
		ctx.wgFinish.Add(1)
		go taskletExecutor(i, ctx, c)
		if c != nil {
			ctx.taskletCtxList = append(ctx.taskletCtxList, c)
		}
	}
}

func releaseExecutors(ctx *TaskCtx) {
	log.Info("Release all executors' ctx")
	for _, c := range ctx.taskletCtxList {
		c.Close()
	}
}

func waitForTaskDone(ctx *TaskCtx) {
	log.Info("Wait for task %q done", ctx.tsk.GetTaskId())
	tskctx.wgFinish.Wait()
}

func handleTaskReq(tsk task.Task) {
	log.Info("Dealing with task %q", tsk.GetTaskId())
	tskctx.kickoff()
	if err := tsk.Init(RUNNING_EXECUTOR_CNT); err == nil {
		tskctx.init()
		prepareExecutors(tskctx, tsk)
		assignTasklets(tskctx, tsk)
		waitForTaskDone(tskctx)
		releaseExecutors(tskctx)
	} else {
		log.Error("Fail to init task %q, %v", tsk.GetTaskId(), err)
		tskctx.setErr(err)
	}
	if tskctx.aborted() {
		tsk.SetError(tskctx.err)
	} else {
		reduceTasklets(tsk, tskctx)
	}
	tskctx.finish()
	report := generateTaskReport(tskctx)
	tskctx.setFree()
	go sendTaskReport(report)
}

func assignTasklets(ctx *TaskCtx, tsk task.Task) {
	log.Info("Assign tasklets")
	i := 0
	for {
		if ctx.aborted() {
			log.Info("Abort assign tasklets")
			break
		}
		taskletid := fmt.Sprintf("%s-%d", tsk.GetTaskId(), i)
		tasklet := tsk.GetNextTasklet(taskletid)
		if tasklet == nil {
			close(ctx.todoTasklets)
			break
		}
		log.Info("Put tasklet %q to todo list", tasklet.GetTaskletId())
		ctx.todoTasklets <- tasklet
		i++
	}
	log.Info("Assign tasklets finished")
}

func taskletExecutor(eid int, ctx *TaskCtx, c task.TaskletCtx) {
	var err error
	defer ctx.wgFinish.Done()
	for {
		if ctx.aborted() {
			log.Info("Error set in taskctx, abort executor #%d", eid)
			break
		}
		log.Info("Executor #%d, retrieve todo tasklet...", eid)
		tasklet, ok := <-ctx.todoTasklets
		if !ok {
			log.Info("Todo tasklets drained, exit executor #%d", eid)
			break
		}
		log.Info("Executor #%d execute tasklet %q", eid, tasklet.GetTaskletId())
		for i := 0; i < TASKLET_MAX_RETRY; i++ {
			if err = tasklet.Execute(c); err == nil {
				break
			}
			log.Info("Retry execute tasklet %q", tasklet.GetTaskletId())
		}
		log.Info("Executor #%d execute tasklet %q done", eid, tasklet.GetTaskletId())
		if err != nil {
			log.Info("Fail on tasklet %q, err %v", tasklet.GetTaskletId(), err)
			tskctx.setErr(err)
			break
		}
		ctx.appendDoneTasklet(tasklet)
	}
	log.Info("Executor #%d, exit", eid)
}

func reduceTasklets(tsk task.Task, ctx *TaskCtx) {
	log.Info("Reduce tasklets for task %q", tsk.GetTaskId())
	close(ctx.doneTasklets)
	tasklets := make([]task.Tasklet, 0, len(ctx.doneTasklets))
	for {
		tasklet, ok := <-ctx.doneTasklets
		if !ok {
			break
		}
		tasklets = append(tasklets, tasklet)
	}
	tsk.ReduceTasklets(tasklets)
}

func generateTaskReport(ctx *TaskCtx) *task.TaskReport {
	tsk := ctx.tsk
	status := ctx.getTaskStatus()
	errMsg := ""
	if err := tsk.GetError(); err != nil {
		errMsg = err.Error()
	}
	return &task.TaskReport{
		Err:     errMsg,
		Tid:     tsk.GetTaskId(),
		Kind:    tsk.GetKind(),
		StartTs: ctx.startTs,
		EndTs:   ctx.endTs,
		Status:  status,
		Output:  tsk.GetOutput(),
	}
}

func sendTaskReport(report *task.TaskReport) {
	log.Info("Send out task report for %q", report.Tid)
	u := workerSelf.makeMasterUrl(uri.MasterWorkerTaskReportUri)
	if _, err := util.HttpPostData(u, report); err == nil {
		log.Info("Send out task report for %q done", report.Tid)
	} else {
		// TODO need retry on error
		log.Error("Send out task report for %q failed, %v", report.Tid, err)
	}
}

func makeTaskspec(r *http.Request) (tspec *task.TaskSpec, err error) {
	buf, err := util.HttpReadRequestJsonBody(r)
	if err != nil {
		err = fmt.Errorf("Fail to read request body, %v", err)
		return
	}
	maxlen := util.Min(len(buf), 2*1024)
	log.Info("Get task spec:\n%s", string(buf[:maxlen]))
	tspec = new(task.TaskSpec)
	if err = json.Unmarshal(buf, tspec); err != nil {
		err = fmt.Errorf("Fail unmarshal tspec %s, %v", string(buf), err)
		return
	}
	return
}

func spawnTask(tspec *task.TaskSpec) (task.Task, error) {
	gen := taskreg.GetTaskGenerator(tspec.Kind)
	if gen == nil {
		return nil, fmt.Errorf("Task %q not supported", tspec.Kind)
	}
	tsk, err := gen(tspec)
	if err != nil {
		return nil, err
	}
	log.Info("Spawn task %q done", tsk.GetTaskId())
	return tsk, nil
}

func taskRecepiant(tspec *task.TaskSpec) error {
	tsk, err := spawnTask(tspec)
	if err != nil {
		return err
	}
	if err := tskctx.checkAndUnsetFree(tsk); err != nil {
		return err
	}
	go handleTaskReq(tsk)
	return nil
}

func taskRecipiantHandler(w http.ResponseWriter, r *http.Request) {
	tspec, err := makeTaskspec(r)
	if err != nil {
		log.Info("Fail to make task spec, %v", err)
		server.FmtResp(w, err, "")
		return
	}
	if err = taskRecepiant(tspec); err != nil {
		log.Info("Can't recieve task %q, %v", tspec.Tid, err)
	}
	server.FmtResp(w, err, "")
}

func reportTaskStatus() {
	taskStatus := tskctx.getTaskStatus()
	if taskStatus == nil {
		return
	}
	u := workerSelf.makeMasterUrl(uri.MasterWorkerTaskStatusUri)
	if _, err := util.HttpPostData(u, taskStatus); err != nil {
		log.Error("Fail to post task status, %v", err)
	}
}

func init() {
	tskctx.free = true
}
