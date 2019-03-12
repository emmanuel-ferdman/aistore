// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/json-iterator/go"
)

type dlResponse struct {
	body       []byte
	statusCode int
	err        error
}

///////////
// PROXY //
///////////

func (p *proxyrunner) targetDownloadRequest(method string, si *cluster.Snode, msg interface{}) dlResponse {
	query := url.Values{}
	query.Add(cmn.URLParamProxyID, p.si.DaemonID)
	query.Add(cmn.URLParamUnixTime, strconv.FormatInt(int64(time.Now().UnixNano()), 10))

	body, err := jsoniter.Marshal(msg)
	if err != nil {
		return dlResponse{
			body:       nil,
			statusCode: http.StatusInternalServerError,
			err:        err,
		}
	}
	args := callArgs{
		si: si,
		req: reqArgs{
			method: method,
			path:   cmn.URLPath(cmn.Version, cmn.Download),
			query:  query,
			body:   body,
		},
		timeout: defaultTimeout,
	}
	res := p.call(args)
	return dlResponse{
		body:       res.outjson,
		statusCode: res.status,
		err:        res.err,
	}
}

func (p *proxyrunner) broadcastDownloadRequest(method string, msg *cmn.DlAdminBody) (string, error) {
	var (
		smap        = p.smapowner.get()
		wg          = &sync.WaitGroup{}
		targetCnt   = len(smap.Tmap)
		responsesCh = make(chan dlResponse, targetCnt)
	)

	for _, si := range smap.Tmap {
		wg.Add(1)
		go func(si *cluster.Snode) {
			responsesCh <- p.targetDownloadRequest(method, si, msg)
			wg.Done()
		}(si)
	}

	wg.Wait()
	close(responsesCh)

	// FIXME: consider adding new stats: downloader failures
	responses := make([]dlResponse, 0, 10)
	for resp := range responsesCh {
		responses = append(responses, resp)
	}

	notFoundCnt := 0
	errors := make([]dlResponse, 0, 10) // errors other than than 404 (not found)
	validResponses := responses[:0]
	for _, resp := range responses {
		if resp.statusCode >= http.StatusBadRequest {
			if resp.statusCode == http.StatusNotFound {
				notFoundCnt++
			} else {
				errors = append(errors, resp)
			}
		} else {
			validResponses = append(validResponses, resp)
		}
	}

	if notFoundCnt == len(responses) { // all responded with 404
		return "", responses[0].err
	} else if len(errors) > 0 {
		return "", errors[0].err
	}

	switch method {
	case http.MethodGet:
		stats := make([]cmn.DlStatusResp, len(validResponses))
		for i, resp := range validResponses {
			err := jsoniter.Unmarshal(resp.body, &stats[i])
			cmn.AssertNoErr(err)
		}

		finished, total := 0, 0
		for _, stat := range stats {
			finished += stat.Finished
			total += stat.Total
		}

		pct := float64(finished) / float64(total) * 100
		return fmt.Sprintf("Status: [finished: %d, total: %d, pct: %.3f%%]", finished, total, pct), nil
	case http.MethodDelete:
		return string(responses[0].body), nil
	default:
		cmn.AssertMsg(false, method)
		return "", nil
	}
}

// objects is a map of objnames (keys) where the corresponding
// value is the link that the download will be saved as.
func (p *proxyrunner) bulkDownloadProcessor(id, bucket, bckProvider, timeout string, objects cmn.SimpleKVs) error {
	var (
		smap  = p.smapowner.get()
		wg    = &sync.WaitGroup{}
		errCh chan error
	)

	bulkTargetRequest := make(map[*cluster.Snode]*cmn.DlBody, smap.CountTargets())
	for objname, link := range objects {
		si, errstr := hrwTarget(bucket, objname, smap)
		if errstr != "" {
			return fmt.Errorf(errstr)
		}

		dlObj := cmn.DlObj{
			Objname: objname,
			Link:    link,
		}

		b, ok := bulkTargetRequest[si]
		if !ok {
			dlBody := &cmn.DlBody{
				ID: id,
			}
			dlBody.Bucket = bucket
			dlBody.BckProvider = bckProvider
			dlBody.Timeout = timeout

			bulkTargetRequest[si] = dlBody
			b = dlBody
		}

		b.Objs = append(b.Objs, dlObj)
	}

	errCh = make(chan error, len(bulkTargetRequest))
	for si, dlBody := range bulkTargetRequest {
		wg.Add(1)
		go func(si *cluster.Snode, dlBody *cmn.DlBody) {
			if resp := p.targetDownloadRequest(http.MethodPost, si, dlBody); resp.err != nil {
				errCh <- resp.err
			}
			wg.Done()
		}(si, dlBody)
	}
	wg.Wait()
	close(errCh)

	// FIXME: consider adding new stats: downloader failures
	failures := make([]error, 0, 10)
	for err := range errCh {
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		glog.Error(failures, len(failures))
		return fmt.Errorf("following downloads failed: %v", failures)
	}
	return nil
}

// [METHOD] /v1/download
func (p *proxyrunner) downloadHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodDelete:
		p.httpDownloadAdmin(w, r)
	case http.MethodPost:
		p.httpDownloadPost(w, r)
	default:
		cmn.InvalidHandlerWithMsg(w, r, "invalid method for /download path")
	}
}

// httpDownloadAdmin is meant for cancelling and getting status updates for
// downloads.
// GET /v1/download?id=...
// DELETE /v1/download?id=...
func (p *proxyrunner) httpDownloadAdmin(w http.ResponseWriter, r *http.Request) {
	var (
		payload = &cmn.DlAdminBody{}
	)

	payload.InitWithQuery(r.URL.Query())
	if err := payload.Validate(); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	glog.V(4).Infof("httpDownloadAdmin payload %v", payload)
	resp, err := p.broadcastDownloadRequest(r.Method, payload)
	if err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	w.Write([]byte(resp))
}

// POST /v1/download
func (p *proxyrunner) httpDownloadPost(w http.ResponseWriter, r *http.Request) {
	apitems, err := p.checkRESTItems(w, r, 1, true, cmn.Version, cmn.Download)
	if err != nil {
		return
	}

	id, err := cmn.GenUUID()
	if err != nil {
		glog.Error(err)
		p.invalmsghdlr(w, r, "failed to generate id for the request", http.StatusInternalServerError)
		return
	}

	if len(apitems) >= 1 {
		switch apitems[0] {
		case cmn.DownloadSingle:
			p.singleDownloadHandler(w, r, id)
			return
		case cmn.DownloadRange:
			p.rangeDownloadHandler(w, r, id)
			return
		case cmn.DownloadMulti:
			p.multiDownloadHandler(w, r, id)
			return
		case cmn.DownloadBucket:
			p.bucketDownloadHandler(w, r, id)
			return
		}
	}
	p.invalmsghdlr(w, r, fmt.Sprintf("%q is not a valid download request path", apitems))
}

// POST /v1/download/single?bucket=...&link=...&objname=...
func (p *proxyrunner) singleDownloadHandler(w http.ResponseWriter, r *http.Request, id string) {
	var (
		payload = &cmn.DlSingle{}
		// link -> objname
		objects = make(cmn.SimpleKVs)
	)

	payload.InitWithQuery(r.URL.Query())
	if err := payload.Validate(); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	glog.V(4).Infof("singleDownloadHandler payload %v", payload)

	if _, ok := p.validateBucket(w, r, payload.Bucket, payload.BckProvider); !ok {
		return
	}

	objects[payload.Objname] = payload.Link
	if err := p.bulkDownloadProcessor(id, payload.Bucket, payload.BckProvider, payload.Timeout, objects); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	w.Write([]byte(id))
}

// POST /v1/download/range?bucket=...&base=...&template=...
func (p *proxyrunner) rangeDownloadHandler(w http.ResponseWriter, r *http.Request, id string) {
	var (
		payload = &cmn.DlRangeBody{}
		// link -> objname
		objects = make(cmn.SimpleKVs)
	)

	payload.InitWithQuery(r.URL.Query())
	if err := payload.Validate(); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	if _, ok := p.validateBucket(w, r, payload.Bucket, payload.BckProvider); !ok {
		return
	}

	glog.V(4).Infof("rangeDownloadHandler payload: %s", payload)

	prefix, suffix, start, end, step, digitCount, err := cmn.ParseBashTemplate(payload.Template)
	if err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	for i := start; i <= end; i += step {
		objname := fmt.Sprintf("%s%0*d%s", prefix, digitCount, i, suffix)
		objects[objname] = payload.Base + objname
	}

	if err := p.bulkDownloadProcessor(id, payload.Bucket, payload.BckProvider, payload.Timeout, objects); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	w.Write([]byte(id))
}

// POST /v1/download/multi?bucket=...&timeout=...
func (p *proxyrunner) multiDownloadHandler(w http.ResponseWriter, r *http.Request, id string) {
	var (
		payload = &cmn.DlMultiBody{}
		objects = make(cmn.SimpleKVs)

		objectsPayload interface{}
	)

	if err := cmn.ReadJSON(w, r, &objectsPayload); err != nil {
		return
	}

	payload.InitWithQuery(r.URL.Query())
	if err := payload.Validate(); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	glog.V(4).Infof("multiDownloadHandler payload: %s", payload)

	if _, ok := p.validateBucket(w, r, payload.Bucket, payload.BckProvider); !ok {
		return
	}

	switch ty := objectsPayload.(type) {
	case map[string]interface{}:
		for key, val := range ty {
			switch v := val.(type) {
			case string:
				objects[key] = v
			default:
				p.invalmsghdlr(w, r, fmt.Sprintf("values in map should be strings, found: %T", v))
				return
			}
		}
	case []interface{}:
		// process list of links
		for _, val := range ty {
			switch link := val.(type) {
			case string:
				objName := path.Base(link)
				if objName == "." || objName == "/" {
					// should we continue and let the use worry about this after?
					p.invalmsghdlr(w, r, fmt.Sprintf("can not extract a valid `object name` from the provided download link: %q.", link))
					return
				}
				objects[objName] = link
			default:
				p.invalmsghdlr(w, r, fmt.Sprintf("values in array should be strings, found: %T", link))
				return
			}
		}
	default:
		p.invalmsghdlr(w, r, fmt.Sprintf("JSON body should be map (string -> string) or array of strings, found: %T", ty))
		return
	}

	// process the downloads
	if err := p.bulkDownloadProcessor(id, payload.Bucket, payload.BckProvider, payload.Timeout, objects); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	w.Write([]byte(id))
}

// POST /v1/download/bucket/name?provider=...&prefix=...&suffix=...
func (p *proxyrunner) bucketDownloadHandler(w http.ResponseWriter, r *http.Request, id string) {
	var (
		payload = &cmn.DlBucketBody{}
		query   = r.URL.Query()
	)

	apiItems, err := p.checkRESTItems(w, r, 1, false, cmn.Version, cmn.Download, cmn.DownloadBucket)
	if err != nil {
		return
	}

	payload.Bucket = apiItems[0]
	payload.InitWithQuery(query)
	if err := payload.Validate(); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	if bckIsLocal, ok := p.validateBucket(w, r, payload.Bucket, payload.BckProvider); !ok {
		return
	} else if bckIsLocal {
		p.invalmsghdlr(w, r, "/download/bucket requires cloud bucket")
		return
	}

	msg := cmn.GetMsg{
		GetPrefix:     payload.Prefix,
		GetPageMarker: "",
		GetFast:       true,
	}

	bckEntries := make([]*cmn.BucketEntry, 0, 1024)
	for {
		curBckEntries, err := p.listBucket(r, payload.Bucket, payload.BckProvider, msg)
		if err != nil {
			p.invalmsghdlr(w, r, err.Error())
			return
		}

		// filter only with matching suffix
		for _, entry := range curBckEntries.Entries {
			if strings.HasSuffix(entry.Name, payload.Suffix) {
				bckEntries = append(bckEntries, entry)
			}
		}

		msg.GetPageMarker = curBckEntries.PageMarker
		if msg.GetPageMarker == "" {
			break
		}
	}

	objects := make([]string, len(bckEntries))
	for idx, entry := range bckEntries {
		objects[idx] = entry.Name
	}
	actionMsg := &cmn.ActionMsg{
		Action: cmn.ActPrefetch,
		Name:   "download/bucket",
		Value:  map[string]interface{}{"objnames": objects},
	}
	if err := p.listRange(http.MethodPost, payload.Bucket, actionMsg, nil); err != nil {
		p.invalmsghdlr(w, r, err.Error())
		return
	}

	w.Write([]byte(id))
}

////////////
// TARGET //
////////////

// NOTE: This request is internal so we can have asserts there.
// [METHOD] /v1/download
func (t *targetrunner) downloadHandler(w http.ResponseWriter, r *http.Request) {
	if !t.verifyProxyRedirection(w, r, "", "", cmn.Download) {
		return
	}

	var (
		response   interface{}
		err        error
		statusCode int
	)

	downloader, err := t.xactions.renewDownloader(t)
	if err != nil {
		t.invalmsghdlr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}

	switch r.Method {
	case http.MethodPost:
		payload := &cmn.DlBody{}
		if err := cmn.ReadJSON(w, r, payload); err != nil {
			return
		}
		cmn.AssertNoErr(payload.Validate())

		glog.V(4).Infof("Downloading: %s", payload)
		response, err, statusCode = downloader.Download(payload)
	case http.MethodGet:
		payload := &cmn.DlAdminBody{}
		if err := cmn.ReadJSON(w, r, payload); err != nil {
			return
		}
		cmn.AssertNoErr(payload.Validate())

		glog.V(4).Infof("Getting status of download: %s", payload)
		response, err, statusCode = downloader.Status(payload.ID)
	case http.MethodDelete:
		payload := &cmn.DlAdminBody{}
		if err := cmn.ReadJSON(w, r, payload); err != nil {
			return
		}
		cmn.AssertNoErr(payload.Validate())

		glog.V(4).Infof("Cancelling download: %s", payload)
		response, err, statusCode = downloader.Cancel(payload.ID)
	default:
		cmn.AssertMsg(false, r.Method)
		return
	}

	if statusCode >= http.StatusBadRequest {
		cmn.InvalidHandlerWithMsg(w, r, err.Error(), statusCode)
		return
	}

	if response != nil {
		b, err := jsoniter.Marshal(response)
		cmn.AssertNoErr(err)
		w.Write(b)
	}
}
