package main

import (
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

var projctx = new(ProjectCtx)

type ProjMeta struct {
	Name     string
	StartTs  time.Time
	EndTs    time.Time
	err      error
	ErrMsg   string
	Finished bool
	JobMetas []*JobMeta
}

func (pmeta *ProjMeta) init(projName string) *ProjMeta {
	pmeta.Name = projName
	return pmeta
}

func (pmeta *ProjMeta) insertJobMeta(jmeta *JobMeta) {
	pmeta.JobMetas = append(pmeta.JobMetas, jmeta)
}

func (pmeta *ProjMeta) snapshot() *ProjMeta {
	metas := make([]*JobMeta, len(pmeta.JobMetas))
	for i, jmeta := range pmeta.JobMetas {
		metas[i] = jmeta
	}
	return &ProjMeta{
		Name:     pmeta.Name,
		StartTs:  pmeta.StartTs,
		EndTs:    pmeta.EndTs,
		ErrMsg:   pmeta.ErrMsg,
		Finished: pmeta.Finished,
		JobMetas: metas,
	}
}

type ProjectCtx struct {
	idx int
	// Following fields under mutex protection
	mutex    sync.Mutex
	free     bool
	projId   string
	config   string
	proj     task.Project
	projMeta *ProjMeta
}

func (ctx *ProjectCtx) init() {
	ctx.free = true
}

func (ctx *ProjectCtx) start() {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	ctx.projMeta = new(ProjMeta).init(ctx.proj.GetName())
	ctx.projMeta.StartTs = time.Now()
}

func (ctx *ProjectCtx) checkAndUnsetFree(proj task.Project, config string) (string, error) {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	if !ctx.free {
		return "", fmt.Errorf("Project %q in running", ctx.projId)
	}
	ctx.free = false
	ctx.proj = proj
	ctx.config = config
	ctx.projId = ctx.makeProjId()
	return ctx.projId, nil
}

func (ctx *ProjectCtx) finish(err error) {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	if err != nil {
		ctx.projMeta.err = err
		ctx.projMeta.ErrMsg = err.Error()
	}
	ctx.projMeta.Finished = true
	ctx.projMeta.EndTs = time.Now()
	ctx.free = true
}

func (ctx *ProjectCtx) makeProjId() string {
	ts := time.Now().UnixNano()
	pid := fmt.Sprintf("proj%d-%d", ts, ctx.idx)
	ctx.idx++
	return pid
}

func (ctx *ProjectCtx) insertJobMeta(jmeta *JobMeta) {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	ctx.projMeta.insertJobMeta(jmeta)
}

func (ctx *ProjectCtx) snapshotProjMeta() *ProjMeta {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	if ctx.projMeta == nil {
		return nil
	}
	return ctx.projMeta.snapshot()
}

func projRunner() {
	log.Info("Run project %q", projctx.projId)
	projctx.start()
	proj := projctx.proj
	if err := proj.Init(projctx.config); err != nil {
		projctx.finish(err)
		log.Error("Fail on project %q init, %v", projctx.projId, err)
		return
	}
	for _, job := range proj.GetJobs() {
		jmeta, err := runJob(job, proj.GetEnv())
		projctx.insertJobMeta(jmeta)
		if err != nil {
			err = fmt.Errorf("Fail on job %q, %v", job.GetKind(), err)
			projctx.finish(err)
			log.Error(err.Error())
			break
		}
	}
	if err := proj.Finish(); err != nil {
		projctx.finish(err)
		log.Error("Fail on project %q finish, %v", projctx.projId, err)
		return
	}
	projctx.finish(nil)
	log.Info("Run project %q finished", projctx.projId)
}

type RunProjReceipt struct {
	ErrMsg string
	ProjId string
}

func runProj(proj task.Project, config string) *RunProjReceipt {
	projId, err := projctx.checkAndUnsetFree(proj, config)
	if err != nil {
		return &RunProjReceipt{
			ErrMsg: err.Error(),
			ProjId: projId,
		}
	}
	go projRunner()
	return &RunProjReceipt{ProjId: projId}
}

func runProjHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		err = fmt.Errorf("Fail to parse form, err %v", err)
		server.FmtResp(w, err, nil)
		return
	}
	projName := r.Form.Get(uri.MasterProjNameKey)
	body, err := util.HttpReadRequestJsonBody(r)
	if err != nil {
		err = fmt.Errorf("Fail to read body, err %v", err)
		server.FmtResp(w, err, nil)
		return
	}
	config := string(body)
	proj := taskreg.GetProj(projName)
	if proj == nil {
		err = fmt.Errorf("Proj %q not supported", projName)
		server.FmtResp(w, err, nil)
		return
	}
	receipt := runProj(proj, config)
	server.FmtResp(w, nil, receipt)
}

func queryProjStatusHandler(w http.ResponseWriter, r *http.Request) {
	pmeta := projctx.snapshotProjMeta()
	jmeta := jobctx.snapshotJobMeta()
	jmetas := pmeta.JobMetas
	if jmeta.Kind == "" {
		// do nothing
	} else if len(jmetas) > 0 {
		last := jmetas[len(jmetas)-1]
		if last.Kind != jmeta.Kind && last.StartTs != jmeta.StartTs {
			pmeta.JobMetas = append(pmeta.JobMetas, jmeta)
		}
	} else {
		pmeta.JobMetas = append(pmeta.JobMetas, jmeta)
	}
	server.FmtResp(w, nil, pmeta)
}

func init() {
	projctx.init()
}
