// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/downloader"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/nl"
	"github.com/NVIDIA/aistore/query"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xaction/xreg"
	jsoniter "github.com/json-iterator/go"
)

////////////////////////////////
// notification receiver      //
// see also cluster/notif.go //
//////////////////////////////

// TODO: cmn.UponProgress as periodic (byte-count, object-count)
// TODO: batch housekeeping for pending notifications
// TODO: add an option to enforce 'if one notifier fails all fail'
// TODO: housekeeping: broadcast in a separate goroutine

// notification category
const (
	notifsName       = ".notifications.prx"
	notifsHousekeepT = 2 * time.Minute
	notifsRemoveMult = 3 // time-to-keep multiplier (time = notifsRemoveMult * notifsHousekeepT)
)

type (
	listeners struct {
		sync.RWMutex
		m map[string]nl.NotifListener // [UUID => NotifListener]
	}

	notifs struct {
		p       *proxyrunner
		nls     *listeners // running
		fin     *listeners // finished
		smapVer int64
	}
	// TODO: simplify using alternate encoding formats (e.g. GOB)
	jsonNotifs struct {
		Running  []*notifListenMsg `json:"running"`
		Finished []*notifListenMsg `json:"finished"`
	}

	nlFilter xreg.XactFilter

	//
	// notification messages
	//

	// receiver to start listening
	// TODO: explore other encoding formats (e.g. GOB) to simplify Marshal and Unmarshal logic
	notifListenMsg struct {
		nl nl.NotifListener
	}
	jsonNL struct {
		Type string              `json:"type"`
		NL   jsoniter.RawMessage `json:"nl"`
	}
)

// interface guard
var _ cluster.Slistener = (*notifs)(nil)

///////////////
// listeners //
///////////////

func newListeners() *listeners {
	return &listeners{m: make(map[string]nl.NotifListener, 64)}
}

func (l *listeners) entry(uuid string) (entry nl.NotifListener, exists bool) {
	l.RLock()
	entry, exists = l.m[uuid]
	l.RUnlock()
	return
}

func (l *listeners) add(nl nl.NotifListener, locked bool) (exists bool) {
	if !locked {
		l.Lock()
	}
	if _, exists = l.m[nl.UUID()]; !exists {
		l.m[nl.UUID()] = nl
	}
	if !locked {
		l.Unlock()
	}
	return
}

func (l *listeners) del(nl nl.NotifListener, locked bool) (ok bool) {
	if !locked {
		l.Lock()
	} else {
		debug.AssertRWMutex(&l.RWMutex, debug.MtxLocked)
	}
	if _, ok = l.m[nl.UUID()]; ok {
		delete(l.m, nl.UUID())
	}
	if !locked {
		l.Unlock()
	}
	return
}

// returns a listener that matches the filter condition.
// for finished xaction listeners, returns latest listener (i.e. having highest finish time)
func (l *listeners) find(flt nlFilter) (nl nl.NotifListener, exists bool) {
	l.RLock()
	defer l.RUnlock()

	var ftime int64
	for _, listener := range l.m {
		if listener.EndTime() < ftime {
			continue
		}
		if flt.match(listener) {
			ftime = listener.EndTime()
			nl, exists = listener, true
		}
		if exists && !listener.Finished() {
			return
		}
	}
	return
}

func (l *listeners) merge(msgs []*notifListenMsg) {
	l.Lock()
	defer l.Unlock()

	for _, m := range msgs {
		if _, ok := l.m[m.nl.UUID()]; !ok {
			l.m[m.nl.UUID()] = m.nl
			m.nl.SetAddedTime()
		}
	}
}

////////////
// notifs //
////////////

func (n *notifs) init(p *proxyrunner) {
	n.p = p
	n.nls = newListeners()
	n.fin = newListeners()
	hk.Reg(notifsName+".gc", n.housekeep, notifsHousekeepT)
	n.p.Sowner().Listeners().Reg(n)
}

func (n *notifs) String() string { return notifsName }

// start listening
func (n *notifs) add(nl nl.NotifListener) {
	cmn.Assert(nl.UUID() != "")
	if exists := n.nls.add(nl, false /*locked*/); exists {
		return
	}
	nl.SetAddedTime()
	glog.Infoln("add " + nl.String())
}

func (n *notifs) del(nl nl.NotifListener, locked bool) (ok bool) {
	ok = n.nls.del(nl, locked /*locked*/)
	if ok {
		glog.Infoln("del " + nl.String())
	}
	return
}

func (n *notifs) entry(uuid string) (nl.NotifListener, bool) {
	entry, exists := n.nls.entry(uuid)
	if exists {
		return entry, true
	}
	entry, exists = n.fin.entry(uuid)
	if exists {
		return entry, true
	}
	return nil, false
}

func (n *notifs) find(flt nlFilter) (nl nl.NotifListener, exists bool) {
	if flt.ID != "" {
		return n.entry(flt.ID)
	}
	nl, exists = n.nls.find(flt)
	if exists || (flt.OnlyRunning != nil && *flt.OnlyRunning) {
		return
	}
	nl, exists = n.fin.find(flt)
	return
}

// verb /v1/notifs/[progress|finished]
func (n *notifs) handler(w http.ResponseWriter, r *http.Request) {
	var (
		notifMsg = &cluster.NotifMsg{}
		nl       nl.NotifListener
		errMsg   error
		uuid     string
		tid      = r.Header.Get(cmn.HeaderCallerID) // sender node ID
		exists   bool
	)
	if r.Method != http.MethodPost {
		cmn.InvalidHandlerWithMsg(w, r, "invalid method for /notifs path")
		return
	}
	apiItems, err := n.p.checkRESTItems(w, r, 1, false, cmn.Version, cmn.Notifs)
	if err != nil {
		return
	}
	if apiItems[0] != cmn.Progress && apiItems[0] != cmn.Finished {
		n.p.invalmsghdlrf(w, r, "Invalid route /notifs/%s", apiItems[0])
		return
	}
	if cmn.ReadJSON(w, r, notifMsg) != nil {
		return
	}

	// NOTE: the sender is asynchronous - ignores the response -
	//       which is why we consider `not-found`, `already-finished`,
	//       and `unknown-notifier` benign non-error conditions
	uuid = notifMsg.UUID
	if !withRetry(func() bool { nl, exists = n.entry(uuid); return exists }) {
		return
	}

	var (
		srcs    = nl.Notifiers()
		tsi, ok = srcs[tid]
	)
	if !ok {
		return
	}
	//
	// NotifListener and notifMsg must have the same type
	//
	nl.RLock()
	if nl.HasFinished(tsi) {
		n.p.invalmsghdlrsilent(w, r, fmt.Sprintf("%s: duplicate %s from %s, %s", n.p.si, notifMsg, tid, nl))
		nl.RUnlock()
		return
	}
	nl.RUnlock()

	if notifMsg.ErrMsg != "" {
		errMsg = errors.New(notifMsg.ErrMsg)
	}

	// NOTE: Default case is not required - will reach here only for valid types.
	switch apiItems[0] {
	// TODO: implement on Started notification
	case cmn.Progress:
		err = n.handleProgress(nl, tsi, notifMsg.Data, errMsg)
	case cmn.Finished:
		err = n.handleFinished(nl, tsi, notifMsg.Data, errMsg)
	}

	if err != nil {
		n.p.invalmsghdlr(w, r, err.Error())
	}
}

func (n *notifs) handleProgress(nl nl.NotifListener, tsi *cluster.Snode, data []byte,
	srcErr error) (err error) {
	nl.Lock()
	defer nl.Unlock()

	if srcErr != nil {
		nl.SetErr(srcErr)
	}
	if data != nil {
		stats, _, _, err := nl.UnmarshalStats(data)
		debug.AssertNoErr(err)
		nl.SetStats(tsi.ID(), stats)
	}
	return
}

func (n *notifs) handleFinished(nl nl.NotifListener, tsi *cluster.Snode, data []byte,
	srcErr error) (err error) {
	var (
		stats   interface{}
		aborted bool
	)

	nl.Lock()
	// data can either be `nil` or a valid encoded stats
	if data != nil {
		stats, _, aborted, err = nl.UnmarshalStats(data)
		debug.AssertNoErr(err)
		nl.SetStats(tsi.ID(), stats)
	}

	done := n.markFinished(nl, tsi, srcErr, aborted)
	nl.Unlock()

	if done {
		n.done(nl)
	}
	return
}

// PRECONDITION: `nl` should be under lock.
func (n *notifs) markFinished(nl nl.NotifListener, tsi *cluster.Snode, srcErr error, aborted bool) (done bool) {
	nl.MarkFinished(tsi)
	if aborted {
		nl.SetAborted()
		if srcErr == nil {
			detail := fmt.Sprintf("%s, node %s", nl, tsi)
			srcErr = cmn.NewAbortedErrorDetails(nl.Kind(), detail)
		}
	}

	if srcErr != nil {
		nl.SetErr(srcErr)
	}
	return nl.AllFinished() || aborted
}

func (n *notifs) done(nl nl.NotifListener) {
	if !n.del(nl, false /*locked*/) {
		// `nl` already removed from active map
		return
	}
	n.fin.add(nl, false /*locked*/)

	if nl.Aborted() {
		config := cmn.GCO.Get()
		// NOTE: we accept finished notifications even after
		// `nl` is aborted. Handle locks carefully.
		args := &bcastArgs{
			req:     nl.AbortArgs(),
			network: cmn.NetworkIntraControl,
			timeout: config.Timeout.MaxKeepalive,
			nodes:   []cluster.NodeMap{nl.Notifiers()},
		}
		args.nodeCount = len(args.nodes[0])
		n.p.bcastToNodesAsync(args)
	}
	nl.Callback(nl, time.Now().UnixNano())
}

//
// housekeeping
//

func (n *notifs) housekeep() time.Duration {
	now := time.Now().UnixNano()
	n.fin.Lock()
	for _, nl := range n.fin.m {
		if time.Duration(now-nl.EndTime()) > notifsRemoveMult*notifsHousekeepT {
			n.fin.del(nl, true /*locked*/)
		}
	}
	n.fin.Unlock()

	if len(n.nls.m) == 0 {
		return notifsHousekeepT
	}
	n.nls.RLock()
	tempn := make(map[string]nl.NotifListener, len(n.nls.m))
	for uuid, nl := range n.nls.m {
		tempn[uuid] = nl
	}
	n.nls.RUnlock()
	for _, nl := range tempn {
		n.syncStats(nl, notifsHousekeepT)
	}
	// cleanup temp cloned notifs
	for u := range tempn {
		delete(tempn, u)
	}
	return notifsHousekeepT
}

func (n *notifs) syncStats(nl nl.NotifListener, dur ...time.Duration) {
	var (
		progressInterval = cmn.GCO.Get().Periodic.NotifTime
		done             bool
	)

	nl.RLock()
	nodesTardy, syncRequired := nl.NodesTardy(dur...)
	nl.RUnlock()
	if !syncRequired {
		return
	}

	args := &bcastArgs{
		network: cmn.NetworkIntraControl,
		timeout: cmn.GCO.Get().Timeout.MaxKeepalive,
	}

	// nodes to fetch stats from
	args.req = nl.QueryArgs()
	args.nodes = []cluster.NodeMap{nodesTardy}
	args.nodeCount = len(args.nodes[0])
	debug.Assert(args.nodeCount > 0) // Ensure that there is at least one node to fetch.

	results := n.p.bcastToNodes(args)
	for res := range results {
		if res.err == nil {
			stats, finished, aborted, err := nl.UnmarshalStats(res.bytes)
			if err != nil {
				glog.Errorf("%s: failed to parse stats from %s, err: %v", n.p.si, res.si, err)
				continue
			}
			nl.Lock()
			if finished {
				done = done || n.markFinished(nl, res.si, nil, aborted)
			}
			nl.SetStats(res.si.ID(), stats)
			nl.Unlock()
		} else if res.status == http.StatusNotFound {
			if mono.Since(nl.AddedTime()) < progressInterval {
				// likely didn't start yet - skipping
				continue
			}
			err := fmt.Errorf("%s: %s not found at %s", n.p.si, nl, res.si)
			nl.Lock()
			done = done || n.markFinished(nl, res.si, err, true) // NOTE: not-found at one ==> all done
			nl.Unlock()
		} else if glog.FastV(4, glog.SmoduleAIS) {
			glog.Errorf("%s: %s, node %s, err: %v", n.p.si, nl, res.si, res.err)
		}
	}

	if done {
		n.done(nl)
	}
}

// Return stats from each node for a given UUID.
func (n *notifs) queryStats(uuid string, durs ...time.Duration) (stats *nl.NodeStats, exists bool) {
	var nl nl.NotifListener
	nl, exists = n.entry(uuid)
	if !exists {
		return
	}
	n.syncStats(nl, durs...)
	stats = nl.NodeStats()
	return
}

func (n *notifs) getOwner(uuid string) (o string, exists bool) {
	var nl nl.NotifListener
	if nl, exists = n.entry(uuid); exists {
		o = nl.GetOwner()
	}
	return
}

// TODO: consider Smap versioning per NotifListener
func (n *notifs) ListenSmapChanged() {
	if !n.p.ClusterStarted() {
		return
	}
	smap := n.p.owner.smap.get()
	if n.smapVer >= smap.Version {
		return
	}
	n.smapVer = smap.Version

	if len(n.nls.m) == 0 {
		return
	}

	var (
		remnl = make(map[string]nl.NotifListener)
		remid = make(cmn.SimpleKVs)
	)
	n.nls.RLock()
	for uuid, nl := range n.nls.m {
		nl.RLock()
		for id := range nl.ActiveNotifiers() {
			if node := smap.GetNode(id); node == nil || node.InMaintenance() {
				remnl[uuid] = nl
				remid[uuid] = id
				break
			}
		}
		nl.RUnlock()
	}
	n.nls.RUnlock()
	if len(remnl) == 0 {
		return
	}
	now := time.Now().UnixNano()
	for uuid, nl := range remnl {
		s := fmt.Sprintf("%s: stop waiting for %s", n.p.si, nl)
		sid := remid[uuid]
		err := &errNodeNotFound{s, sid, n.p.si, smap}
		nl.Lock()
		nl.SetErr(err)
		nl.SetAborted()
		nl.Unlock()
	}
	n.fin.Lock()
	for uuid, nl := range remnl {
		cmn.Assert(nl.UUID() == uuid)
		n.fin.add(nl, true /*locked*/)
	}
	n.fin.Unlock()
	n.nls.Lock()
	for _, nl := range remnl {
		n.del(nl, true /*locked*/)
	}
	n.nls.Unlock()

	for uuid, nl := range remnl {
		nl.Callback(nl, now)
		// cleanup
		delete(remnl, uuid)
		delete(remid, uuid)
	}
}

func (n *notifs) MarshalJSON() (data []byte, err error) {
	t := jsonNotifs{}
	n.nls.RLock()
	n.fin.RLock()
	defer func() {
		n.fin.RUnlock()
		n.nls.RUnlock()
	}()
	t.Running = make([]*notifListenMsg, 0, len(n.nls.m))
	t.Finished = make([]*notifListenMsg, 0, len(n.fin.m))
	for _, nl := range n.nls.m {
		t.Running = append(t.Running, newNLMsg(nl))
	}

	for _, nl := range n.fin.m {
		t.Finished = append(t.Finished, newNLMsg(nl))
	}
	return jsoniter.Marshal(t)
}

func (n *notifs) UnmarshalJSON(data []byte) (err error) {
	t := jsonNotifs{}

	if err = jsoniter.Unmarshal(data, &t); err != nil {
		return
	}
	if len(t.Running) > 0 {
		n.nls.merge(t.Running)
	}

	if len(t.Finished) > 0 {
		n.fin.merge(t.Finished)
	}
	return
}

func newNLMsg(nl nl.NotifListener) *notifListenMsg {
	return &notifListenMsg{nl: nl}
}

func (n *notifListenMsg) MarshalJSON() (data []byte, err error) {
	n.nl.RLock()
	defer n.nl.RUnlock()
	t := jsonNL{Type: n.nl.Kind()}
	t.NL, err = jsoniter.Marshal(n.nl)
	if err != nil {
		return
	}
	return jsoniter.Marshal(t)
}

func (n *notifListenMsg) UnmarshalJSON(data []byte) (err error) {
	t := jsonNL{}
	if err = jsoniter.Unmarshal(data, &t); err != nil {
		return
	}
	if t.Type == cmn.ActQueryObjects {
		n.nl = &query.NotifListenerQuery{}
	} else if isDLType(t.Type) {
		n.nl = &downloader.NotifDownloadListerner{}
	} else {
		n.nl = &xaction.NotifXactListener{}
	}
	err = jsoniter.Unmarshal(t.NL, &n.nl)
	if err != nil {
		return
	}
	return
}

func isDLType(t string) bool {
	return t == string(downloader.DlTypeMulti) ||
		t == string(downloader.DlTypeCloud) ||
		t == string(downloader.DlTypeSingle) ||
		t == string(downloader.DlTypeRange)
}

//
// Notification listener filter (nlFilter)
//

func (nf *nlFilter) match(nl nl.NotifListener) bool {
	if nl.UUID() == nf.ID {
		return true
	}

	if nl.Kind() == nf.Kind {
		if nf.Bck == nil || nf.Bck.IsEmpty() {
			return true
		}
		for _, bck := range nl.Bcks() {
			if cmn.QueryBcks(nf.Bck.Bck).Contains(bck) {
				return true
			}
		}
	}
	return false
}
