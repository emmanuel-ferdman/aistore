/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/stats"
)

// =============================== Summary ====================================
//
// Downloader is a long running task that provides a AIS a means to download
// objects from the internet by providing a URL(referred to as a link) to the
// server where the object exists. Downloader does not make the HTTP GET
// requests to download the files itself- it purely manages the lifecycle of
// joggers and dispatches any download related request to the correct jogger
// instance.
//
// ====== API ======
//
// API exposed to the rest of the code includes the following operations:
//   * Run      - to run
//   * Stop     - to stop
//   * Download    - to download a new object from a URL
//   * Cancel      - to cancel a previously requested download (currently queued or currently downloading)
//   * Status      - to request the status of a previously requested download
// The Download, Cancel and Status requests are encapsulated into an internal
// request object and added to a request queue and then are dispatched to the
// correct jogger. The remaining operations are private to the Downloader and
// are used only internally.
//
// Each jogger, which corresponds to a mountpath, has a download channel
// (downloadCh) where download request that are dispatched from Downloader are
// queued. Thus, downloads occur on a per-mountpath basis and are handled one at
// a time by jogger as they arrive.
//
// ====== Downloading ======
//
// After Downloader receives a download request, adds it to the correct jogger's queue.
// Downloads are represented as extended actions (xactDownload), and there is at
// most one active xactDownload assigned to any jogger at any given time. The
// xactDownload are created when the jogger dequeues a download request from its
// downloadCh and are destroyed when the download is cancelled, finished or
// fails.
//
// After a xactDownload is created, a separate goroutine is spun up to make the
// actual GET request (jogger's download method). The goroutine for the jogger
// sits idle awaiting an abort(failed or cancelled) or finish message from the
// goroutine responsible for the actual download.
//
// ====== Cancelling ======
//
// When Downloader receives a cancel request, it cancels running task or
// if the task is scheduled but is not yet processed, then it is removed
// from queue (see: put, get). If the task is running, cancelFunc is
// invoked to cancel task's request.
//
// ====== Status Updates ======
//
// Status updates are made possible by progressReader, which just overwrites the
// io.Reader's Read method to additionally notify a Reporter Func, that gets
// notified the number of bytes that have been read every time we read from the
// response body from the HTTP GET request we make to to the link to download
// the object.
//
// When Downloader receives a status update request, it dispatches to a separate
// jogger goroutine that checks if the downloaded completed. Otherwise it checks
// if it is currently being downloaded. If it is being currently downloaded, it
// returns the progress. Otherwise, it returns that the object hasn't been
// downloaded yet. Now, the file may never be downloaded if the download was
// never queued to the downloadCh.
//
// Status updates are either reported in terms of size or size and percentage.
// Before downloading an object from a server, we attempt to make a HEAD request
// to obtain the object size using the "Content-Length" field returned in the
// Header. Note: not all servers implement a HEAD request handler. For these
// cases, a progress percentage is not returned, just the current number of
// bytes that have been downloaded.
//
// ====== Notes ======
//
// Downloader assumes that any type of download request is first sent to a proxy
// and then redirected to the correct target's Downloader (the proxy uses the
// HRW algorithm to determine the target). It is not possible to directly hit a
// Target's download endpoint to force an object to be downloaded to that
// Target, all request must go through a proxy first.
//
// ================================ Summary ====================================

const (
	adminCancel  = "CANCEL"
	adminStatus  = "STATUS"
	taskDownload = "DOWNLOAD"
	queueChSize  = 200

	putInQueueTimeout = time.Second * 10
)

var (
	httpClient = &http.Client{
		Timeout: cmn.GCO.Get().Timeout.DefaultLong,
	}
)

// public types
type (
	// Downloader implements the fs.PathRunner and XactDemand interface. When
	// download related requests are made to AIS using the download endpoint,
	// Downloader dispatches these requests to the corresponding jogger.
	Downloader struct {
		cmn.NamedID
		cmn.XactDemandBase

		t          cluster.Target
		mountpaths *fs.MountedFS
		stats      stats.Tracker

		mpathReqCh chan fs.ChangeReq
		adminCh    chan *request
		downloadCh chan *task
		joggers    map[string]*jogger // mpath -> jogger

		db *downloaderDB
	}
)

// private types
type (
	// The result of calling one of Downloader's exposed methos is encapsulated
	// in a response object, which is used to communicate the outcome of the
	// request.
	response struct {
		resp       interface{}
		err        error
		statusCode int
	}

	// Calling Downloader's exposed methods results in the creation of a request
	// for admin related tasks (i.e. cancelling and status updates) and a dlTask
	// for a downlad request. These objects are used by Downloader to process
	// the request, and are then dispatched to the correct jogger to be handled.
	request struct {
		action      string // one of: adminCancel, adminStatus, taskDownload
		id          string // id of the job task
		obj         cmn.DlObj
		bucket      string
		bckProvider string
		timeout     string
		fqn         string         // fqn of the object after it has been committed
		responseCh  chan *response // where the outcome of the request is written
	}

	// task embeds cmn.XactBase, but it is not part of the targetrunner's xactions member
	// variable. Instead, Downloader manages the lifecycle of the extended action.
	task struct {
		parent *Downloader
		*request

		headers     map[string]string // the headers that are forwarded to the get request to download the object.
		currentSize int64             // the current size of the file (updated as the download progresses)
		finishedCh  chan error        // when a jogger finishes downloading a dlTask

		downloadCtx context.Context    // context with cancel function
		cancelFunc  context.CancelFunc // used to cancel the download after the request commences
	}

	queue struct {
		sync.RWMutex
		ch chan *task // for pending downloads
		m  map[string]struct{}
	}

	// Each jogger corresponds to a mpath. All types of download requests
	// corresponding to the jogger's mpath are forwarded to the jogger. Joggers
	// exist in the Downloader's jogger member variable, and run only when there
	// are dlTasks.
	jogger struct {
		mpath       string
		terminateCh chan struct{} // synchronizes termination
		parent      *Downloader

		q *queue

		sync.Mutex
		// lock protected
		task      *task // currently running download task
		stopAgent bool
	}

	progressReader struct {
		r        io.Reader
		reporter func(n int64)
	}
)

//==================================== Requests ===========================================

func (req *request) String() (str string) {
	str += fmt.Sprintf("id: %q, objname: %q, link: %q, ", req.id, req.obj.Objname, req.obj.Link)
	if req.bucket != "" {
		str += fmt.Sprintf("bucket: %q (provider: %q), ", req.bucket, req.bckProvider)
	}

	return "{" + strings.TrimSuffix(str, ", ") + "}"
}

func (req *request) uid() string {
	return fmt.Sprintf("%s|%s|%s", req.obj.Link, req.bucket, req.obj.Objname)
}

func (req *request) write(resp interface{}, err error, statusCode int) {
	req.responseCh <- &response{
		resp:       resp,
		err:        err,
		statusCode: statusCode,
	}
	close(req.responseCh)
}

func (req *request) writeErrResp(err error, statusCode int) {
	req.write(nil, err, statusCode)
}

func (req *request) writeResp(resp interface{}) {
	req.write(resp, nil, http.StatusOK)
}

func (req *request) equals(rhs *request) bool {
	return req.uid() == rhs.uid()
}

// ========================== progressReader ===================================

var _ io.ReadCloser = &progressReader{}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.r.Read(p)
	pr.reporter(int64(n))
	return
}

func (pr *progressReader) Close() error {
	pr.r = nil
	pr.reporter = nil
	return nil
}

// ============================= Downloader ====================================
/*
 * Downloader implements the fs.PathRunner interface
 */

var _ fs.PathRunner = &Downloader{}

func (d *Downloader) ReqAddMountpath(mpath string)     { d.mpathReqCh <- fs.MountpathAdd(mpath) }
func (d *Downloader) ReqRemoveMountpath(mpath string)  { d.mpathReqCh <- fs.MountpathRem(mpath) }
func (d *Downloader) ReqEnableMountpath(mpath string)  {}
func (d *Downloader) ReqDisableMountpath(mpath string) {}

func (d *Downloader) addJogger(mpath string) {
	if _, ok := d.joggers[mpath]; ok {
		glog.Warningf("Attempted to add an already existing mountpath %q", mpath)
		return
	}
	mpathInfo, _ := d.mountpaths.Path2MpathInfo(mpath)
	if mpathInfo == nil {
		glog.Errorf("Attempted to add a mountpath %q with no corresponding filesystem", mpath)
		return
	}
	j := d.newJogger(mpath)
	go j.jog()
	d.joggers[mpath] = j
}

func (d *Downloader) removeJogger(mpath string) {
	jogger, ok := d.joggers[mpath]
	if !ok {
		glog.Errorf("Invalid mountpath %q", mpath)
		return
	}

	delete(d.joggers, mpath)
	jogger.stop()
}

/*
 * Downloader constructors
 */
func NewDownloader(t cluster.Target, stats stats.Tracker, f *fs.MountedFS, id int64, kind string) (d *Downloader, err error) {
	db, err := newDownloadDB()
	if err != nil {
		return nil, err
	}

	return &Downloader{
		XactDemandBase: *cmn.NewXactDemandBase(id, kind, ""),
		t:              t,
		stats:          stats,
		mountpaths:     f,
		mpathReqCh:     make(chan fs.ChangeReq, 1),
		adminCh:        make(chan *request),
		downloadCh:     make(chan *task),
		joggers:        make(map[string]*jogger, 8),
		db:             db,
	}, nil
}

func (d *Downloader) newJogger(mpath string) *jogger {
	return &jogger{
		mpath:       mpath,
		parent:      d,
		q:           newQueue(),
		terminateCh: make(chan struct{}, 1),
	}
}

func (d *Downloader) init() {
	availablePaths, disabledPaths := d.mountpaths.Get()
	for mpath := range availablePaths {
		d.addJogger(mpath)
	}
	for mpath := range disabledPaths {
		d.addJogger(mpath)
	}
}

func (d *Downloader) Run() error {
	glog.Infof("Starting %s", d.Getname())
	d.t.GetFSPRG().Reg(d)
	d.init()
Loop:
	for {
		select {
		case req := <-d.adminCh:
			switch req.action {
			case adminStatus:
				d.dispatchStatus(req)
			case adminCancel:
				d.dispatchCancel(req)
			default:
				cmn.AssertFmt(false, req, req.action)
			}
		case task := <-d.downloadCh:
			d.dispatchDownload(task)
		case mpathRequest := <-d.mpathReqCh:
			switch mpathRequest.Action {
			case fs.Add:
				d.addJogger(mpathRequest.Path)
			case fs.Remove:
				d.removeJogger(mpathRequest.Path)
			}
		case <-d.ChanCheckTimeout():
			if d.Timeout() {
				glog.Infof("%s has timed out. Exiting...", d.Getname())
				break Loop
			}
		case <-d.ChanAbort():
			glog.Infof("%s has been aborted. Exiting...", d.Getname())
			break Loop
		}
	}
	d.Stop(nil)
	return nil
}

// Stop terminates the downloader
func (d *Downloader) Stop(err error) {
	d.t.GetFSPRG().Unreg(d)
	d.XactDemandBase.Stop()
	for _, jogger := range d.joggers {
		jogger.stop()
	}
	d.EndTime(time.Now())
	glog.Infof("Stopped %s", d.Getname())
}

/*
 * Downloader's exposed methods
 */

func (d *Downloader) Download(body *cmn.DlBody) (resp interface{}, err error, statusCode int) {
	d.IncPending()
	defer d.DecPending()

	if err := d.db.setJob(body.ID, body); err != nil {
		return err.Error(), err, http.StatusInternalServerError
	}

	responses := make([]chan *response, 0, len(body.Objs))
	for _, obj := range body.Objs {
		// Invariant: either there was an error before adding to queue, or file
		// was deleted from queue (on cancel or successful run). In both cases
		// we decrease pending.
		d.IncPending()

		rch := make(chan *response, 1)
		responses = append(responses, rch)
		t := &task{
			parent: d,
			request: &request{
				action:      taskDownload,
				id:          body.ID,
				obj:         obj,
				bucket:      body.Bucket,
				bckProvider: body.BckProvider,
				timeout:     body.Timeout,
				responseCh:  rch,
			},
			finishedCh: make(chan error, 1),
		}
		d.downloadCh <- t
	}

	for _, response := range responses {
		// await the response
		r := <-response
		if r.err != nil {
			d.Cancel(body.ID) // cancel whole job
			return r.resp, r.err, r.statusCode
		}
	}
	return nil, nil, http.StatusOK
}

func (d *Downloader) Cancel(id string) (resp interface{}, err error, statusCode int) {
	d.IncPending()
	req := &request{
		action:     adminCancel,
		id:         id,
		responseCh: make(chan *response, 1),
	}
	d.adminCh <- req

	// await the response
	r := <-req.responseCh
	d.DecPending()
	return r.resp, r.err, r.statusCode
}

func (d *Downloader) Status(id string) (resp interface{}, err error, statusCode int) {
	d.IncPending()
	req := &request{
		action:     adminStatus,
		id:         id,
		responseCh: make(chan *response, 1),
	}
	d.adminCh <- req

	// await the response
	r := <-req.responseCh
	d.DecPending()
	return r.resp, r.err, r.statusCode
}

/*
 * Downloader's dispatch methods (forwards request to jogger)
 */

func (d *Downloader) dispatchDownload(t *task) {
	var (
		resp      response
		added, ok bool
		j         *jogger
	)

	lom := &cluster.LOM{T: d.t, Bucket: t.bucket, Objname: t.obj.Objname}
	errstr := lom.Fill(t.bckProvider, cluster.LomFstat)
	if errstr != "" {
		resp.err, resp.statusCode = errors.New(errstr), http.StatusInternalServerError
		goto finalize
	}
	if lom.Exists() {
		resp.resp = fmt.Sprintf("object %q already exists - skipping", t.obj.Objname)
		goto finalize
	}
	t.fqn = lom.FQN

	if lom.ParsedFQN.MpathInfo == nil {
		err := fmt.Errorf("download task with %v failed. Failed to get mountpath for the request's fqn %s", t, t.fqn)
		glog.Error(err.Error())
		resp.err, resp.statusCode = err, http.StatusInternalServerError
		goto finalize
	}
	j, ok = d.joggers[lom.ParsedFQN.MpathInfo.Path]
	cmn.AssertMsg(ok, fmt.Sprintf("no mpath exists for %v", t))

	resp, added = j.putIntoDownloadQueue(t)

finalize:
	t.write(resp.resp, resp.err, resp.statusCode)

	// Following can happen:
	//  * in case of error
	//  * object already exists
	//  * object already in queue
	//
	// In all of these cases we need to decrease pending since it was not added
	// to the queue.
	if !added {
		d.DecPending()
	}
}

func (d *Downloader) dispatchCancel(req *request) {
	body, err := d.db.getJob(req.id)
	if err != nil {
		if err == errJobNotFound {
			req.writeErrResp(fmt.Errorf("download job with id %q has not been found", req.id), http.StatusNotFound)
			return
		}
		req.writeErrResp(err, http.StatusInternalServerError)
		return
	}

	errs := make([]response, 0, len(body.Objs))
	for _, j := range d.joggers {
		j.Lock()
		for _, obj := range body.Objs {
			req.obj = obj
			req.bucket = body.Bucket
			req.bckProvider = body.BckProvider

			lom := &cluster.LOM{T: d.t, Bucket: req.bucket, Objname: req.obj.Objname}
			errstr := lom.Fill(req.bckProvider, cluster.LomFstat)
			if errstr != "" {
				errs = append(errs, response{
					err:        errors.New(errstr),
					statusCode: http.StatusInternalServerError,
				})
				continue
			}
			if lom.Exists() {
				continue
			}

			// Cancel currently running task
			if j.task != nil && j.task.request.equals(req) {
				// Task is running
				j.task.cancel()
				continue
			}

			// If not running but in queue we need to decrease number of pending
			if existed := j.q.delete(req); existed {
				d.DecPending()
			}
		}
		j.Unlock()
	}

	if len(errs) > 0 {
		r := errs[0] // TODO: we should probably print all errors
		req.writeErrResp(r.err, r.statusCode)
		return
	}

	err = d.db.delJob(req.id)
	cmn.AssertNoErr(err) // everything should be okay since getReqFromDB
	req.writeResp(nil)
}

func (d *Downloader) dispatchStatus(req *request) {
	body, err := d.db.getJob(req.id)
	if err != nil {
		if err == errJobNotFound {
			req.writeErrResp(fmt.Errorf("download job with id %q has not been found", req.id), http.StatusNotFound)
			return
		}

		req.writeErrResp(err, http.StatusInternalServerError)
		return
	}

	total := len(body.Objs)
	finished := 0

	errs := make([]response, 0, len(body.Objs))
	for _, obj := range body.Objs {
		lom := &cluster.LOM{T: d.t, Bucket: body.Bucket, Objname: obj.Objname}
		errstr := lom.Fill(body.BckProvider, cluster.LomFstat)
		if errstr != "" {
			errs = append(errs, response{
				err:        err,
				statusCode: http.StatusInternalServerError,
			})
			continue
		}
		if lom.Exists() {
			// It is possible this file already existed on cluster and was never downloaded.
			finished++
			continue
		}
		req.fqn = lom.FQN

		if lom.ParsedFQN.MpathInfo == nil {
			err := fmt.Errorf("status request with %v failed. Failed to obtain mountpath for request's fqn %s", req, req.fqn)
			glog.Error(err.Error())
			errs = append(errs, response{
				err:        err,
				statusCode: http.StatusInternalServerError,
			})
			continue
		}
		_, ok := d.joggers[lom.ParsedFQN.MpathInfo.Path]
		cmn.AssertMsg(ok, fmt.Sprintf("status request with %v failed. No corresponding mpath exists", req))

		// TODO: calculating progress should take into account progress of currently downloaded task
	}

	if len(errs) > 0 {
		r := errs[0] // TODO: we should probably print all errors
		req.writeErrResp(r.err, r.statusCode)
		return
	}

	req.writeResp(cmn.DlStatusResp{
		Finished: finished,
		Total:    total,
	})
}

//==================================== jogger =====================================

func (j *jogger) putIntoDownloadQueue(task *task) (response, bool) {
	cmn.Assert(task != nil)
	added, err, errCode := j.q.put(task)
	if err != nil {
		return response{
			err:        err,
			statusCode: errCode,
		}, false
	}

	return response{
		resp: fmt.Sprintf("Download request %s added to queue", task),
	}, added
}

func (j *jogger) jog() {
	glog.Infof("Starting jogger for mpath %q.", j.mpath)
Loop:
	for {
		t := j.q.get()
		if t == nil {
			break Loop
		}
		j.Lock()
		if j.stopAgent {
			j.Unlock()
			break Loop
		}

		j.task = t
		j.Unlock()

		// start download
		go t.download()

		// await abort or completion
		if err := <-t.waitForFinish(); err != nil {
			glog.Errorf("error occurred when downloading %s: %v", t, err)
		}
		j.Lock()
		j.task = nil
		j.Unlock()
		if exists := j.q.delete(t.request); exists {
			j.parent.DecPending()
		}
	}

	j.q.cleanup()
	j.terminateCh <- struct{}{}
}

// Stop terminates the jogger
func (j *jogger) stop() {
	glog.Infof("Stopping jogger for mpath: %s", j.mpath)
	j.q.stop()

	j.Lock()
	j.stopAgent = true
	if j.task != nil {
		j.task.abort(errors.New("stopped jogger"))
	}
	j.Unlock()

	<-j.terminateCh
}

func (t *task) download() {
	lom := &cluster.LOM{T: t.parent.t, Bucket: t.bucket, Objname: t.obj.Objname}
	if errstr := lom.Fill(t.bckProvider, cluster.LomFstat); errstr != "" {
		t.abort(errors.New(errstr))
		return
	}
	if lom.Exists() {
		t.abort(errors.New("object with the same bucket and objname already exists"))
		return
	}
	postFQN := lom.GenFQN(fs.WorkfileType, fs.WorkfilePut)

	// create request
	httpReq, err := http.NewRequest(http.MethodGet, t.obj.Link, nil)
	if err != nil {
		t.abort(err)
		return
	}

	// add headers
	if len(t.headers) > 0 {
		for k, v := range t.headers {
			httpReq.Header.Set(k, v)
		}
	}

	requestWithContext := httpReq.WithContext(t.downloadCtx)
	if glog.V(4) {
		glog.Infof("Starting download for %v", t)
	}
	started := time.Now()
	response, err := httpClient.Do(requestWithContext)
	if err != nil {
		t.abort(err)
		return
	}
	if response.StatusCode >= http.StatusBadRequest {
		t.abort(fmt.Errorf("status code: %d", response.StatusCode))
		return
	}

	// Create a custom reader to monitor progress every time we read from response body stream
	progressReader := &progressReader{
		r: response.Body,
		reporter: func(n int64) {
			atomic.AddInt64(&t.currentSize, n)
		},
	}

	if err := t.parent.t.Receive(postFQN, progressReader, lom); err != nil {
		t.abort(err)
		return
	}

	t.parent.stats.AddMany(
		stats.NamedVal64{Name: stats.DownloadSize, Val: t.currentSize},
		stats.NamedVal64{Name: stats.DownloadLatency, Val: int64(time.Since(started))},
	)
	t.finishedCh <- nil
}

func (t *task) cancel() {
	t.cancelFunc()
}

// TODO: this should also inform somehow downloader status about being aborted/canceled
// Probably we need to extend the persistent database (db.go) so that it will contain
// also information about specific tasks.
func (t *task) abort(err error) {
	t.parent.stats.Add(stats.ErrDownloadCount, 1)
	t.finishedCh <- err
}

func (t *task) waitForFinish() <-chan error {
	return t.finishedCh
}

func (t *task) String() string {
	return t.request.String()
}

func newQueue() *queue {
	return &queue{
		ch: make(chan *task, queueChSize),
		m:  make(map[string]struct{}),
	}
}

func (q *queue) put(t *task) (added bool, err error, errCode int) {
	timer := time.NewTimer(putInQueueTimeout)

	q.Lock()
	defer q.Unlock()
	if _, exists := q.m[t.request.uid()]; exists {
		// If request already exists we should just omit this
		return false, nil, 0
	}

	select {
	case q.ch <- t:
		break
	case <-timer.C:
		return false, fmt.Errorf("timeout when trying to put task %v in queue, try later", t), http.StatusRequestTimeout
	}
	timer.Stop()
	q.m[t.request.uid()] = struct{}{}
	return true, nil, 0
}

// get try to find first task which was not yet canceled
func (q *queue) get() (foundTask *task) {
	for foundTask == nil {
		t, ok := <-q.ch
		if !ok {
			foundTask = nil
			return
		}

		q.RLock()
		if _, exists := q.m[t.request.uid()]; exists {
			// NOTE: We do not delete task here but postpone it until the task
			// has finished to prevent situation where we put task which is being
			// downloaded.
			foundTask = t
		}
		q.RUnlock()
	}

	timeout := cmn.GCO.Get().Downloader.Timeout
	if foundTask.timeout != "" {
		var err error
		timeout, err = time.ParseDuration(foundTask.timeout)
		cmn.AssertNoErr(err) // this should be checked beforehand
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	foundTask.downloadCtx = ctx
	foundTask.cancelFunc = cancel
	return
}

func (q *queue) delete(req *request) bool {
	q.Lock()
	_, exists := q.m[req.uid()]
	delete(q.m, req.uid())
	q.Unlock()
	return exists
}

func (q *queue) stop() {
	q.RLock()
	if q.ch != nil {
		close(q.ch)
	}
	q.RUnlock()
}

func (q *queue) cleanup() {
	q.Lock()
	q.ch = nil
	q.m = nil
	q.Unlock()
}
