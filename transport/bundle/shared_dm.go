// Package bundle provides multi-streaming transport with the functionality
// to dynamically (un)register receive endpoints, establish long-lived flows, and more.
/*
 * Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
 */
package bundle

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/xact"
)

// TODO -- FIXME:
// - rm '|' and parsing; add demux []byte to transport header
const Sepa = "|"

// [TODO]
// - Close() vs usage (when len(rxcbs) > 0); provide xctn.onFinished() => UnregRecv
// - limitation: hdr.Opaque is exclusively reserved xaction ID

type sharedDM struct {
	dm    DM
	rxcbs map[string]transport.RecvObj
	ocmu  sync.Mutex
	rxmu  sync.Mutex
}

// global
var SDM sharedDM

// called upon target startup
func InitSDM(config *cmn.Config, compression string) {
	extra := Extra{Config: config, Compression: compression}
	SDM.dm.init(SDM.trname(), SDM.recv, cmn.OwtNone, extra)
}

func (sdm *sharedDM) isOpen() bool { return sdm.dm.stage.opened.Load() }

func (sdm *sharedDM) IsActive() (active bool) {
	sdm.rxmu.Lock()
	active = len(sdm.rxcbs) > 0
	sdm.rxmu.Unlock()
	return
}

// constant (until and unless we run multiple shared-DMs)
func (*sharedDM) trname() string { return "shared-dm" }

func (sdm *sharedDM) _already() {
	nlog.WarningDepth(2, core.T.String(), sdm.trname(), "is already open")
}

// called on-demand
func (sdm *sharedDM) Open() error {
	if sdm.isOpen() {
		sdm._already()
		return nil
	}

	sdm.ocmu.Lock()
	if sdm.isOpen() {
		sdm.ocmu.Unlock()
		sdm._already()
		return nil
	}

	sdm.rxmu.Lock()
	sdm.rxcbs = make(map[string]transport.RecvObj, 4)
	sdm.rxmu.Unlock()

	if err := sdm.dm.RegRecv(); err != nil {
		sdm.ocmu.Unlock()
		nlog.ErrorDepth(1, core.T.String(), err)
		debug.AssertNoErr(err)
		return err
	}
	sdm.dm.Open()
	sdm.ocmu.Unlock()

	nlog.InfoDepth(1, core.T.String(), "open", sdm.trname())
	return nil
}

// nothing running + 10m inactivity
func (sdm *sharedDM) Close() error {
	if !sdm.isOpen() {
		return nil
	}
	sdm.ocmu.Lock()
	if !sdm.isOpen() {
		sdm.ocmu.Unlock()
		return nil
	}

	var (
		xid string
		l   int
	)
	sdm.rxmu.Lock()
	for xid = range sdm.rxcbs {
		break
	}
	l = len(sdm.rxcbs)

	if l > 0 {
		sdm.rxmu.Unlock()
		sdm.ocmu.Unlock()
		debug.Assert(cos.IsValidUUID(xid), xid)
		return fmt.Errorf("cannot close %s: [%s, %d]", sdm.trname(), xid, l)
	}

	sdm.rxcbs = nil
	sdm.rxmu.Unlock()

	sdm.dm.Close(nil)
	sdm.dm.UnregRecv()
	sdm.ocmu.Unlock()

	nlog.InfoDepth(1, core.T.String(), "close", sdm.trname())
	return nil
}

func (sdm *sharedDM) RegRecv(xid string, cb transport.RecvObj) {
	sdm.ocmu.Lock()
	sdm.rxmu.Lock()
	if !sdm.isOpen() {
		sdm.rxmu.Unlock()
		sdm.ocmu.Unlock()
		debug.Assert(false, sdm.trname(), " ", "closed")
		return
	}
	debug.Assert(sdm.rxcbs[xid] == nil)
	sdm.rxcbs[xid] = cb
	sdm.rxmu.Unlock()
	sdm.ocmu.Unlock()
}

func (sdm *sharedDM) UnregRecv(xid string) {
	sdm.ocmu.Lock()
	sdm.rxmu.Lock()
	if !sdm.isOpen() {
		sdm.rxmu.Unlock()
		sdm.ocmu.Unlock()
		debug.Assert(false, sdm.trname(), " ", "closed")
		return
	}
	delete(sdm.rxcbs, xid)
	sdm.rxmu.Unlock()
	sdm.ocmu.Unlock()
}

func (sdm *sharedDM) Send(obj *transport.Obj, roc cos.ReadOpenCloser, tsi *meta.Snode) error {
	return sdm.dm.Send(obj, roc, tsi)
}

func (sdm *sharedDM) recv(hdr *transport.ObjHdr, r io.Reader, err error) error {
	if err != nil {
		return err
	}

	// TODO(xid-demux): remove '|' parsing; use cos.UnsafeS(hdr.Opaque) as is and don't change the latter
	xid := string(hdr.Opaque)
	if i := strings.Index(xid, Sepa); i > 0 {
		xid = xid[:i]
		hdr.Opaque = hdr.Opaque[i+1:]
	}

	if err := xact.CheckValidUUID(xid); err != nil {
		return fmt.Errorf("%s: %v", sdm.trname(), err)
	}

	sdm.rxmu.Lock()
	if !sdm.isOpen() {
		sdm.rxmu.Unlock()
		return fmt.Errorf("%s is closed, dropping recv [xid: %s, oname: %s]", sdm.trname(), xid, hdr.ObjName)
	}
	cb, ok := sdm.rxcbs[xid]
	sdm.rxmu.Unlock()

	if !ok {
		return fmt.Errorf("%s: xid %s not found, dropping recv [oname: %s]", sdm.trname(), xid, hdr.ObjName)
	}
	return cb(hdr, r, nil)
}
