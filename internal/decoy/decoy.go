// decoy.go - Katzenpost server decoy traffic.
// Copyright (C) 2018  Yawning Angel.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package decoy implements the decoy traffic source and sink.
package decoy

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	mRand "math/rand"
	"sync"
	"time"

	"git.schwanenlied.me/yawning/avl.git"
	"github.com/hashcloak/Meson-client/pkiclient/epochtime"
	internalConstants "github.com/hashcloak/Meson-server/internal/constants"
	"github.com/hashcloak/Meson-server/internal/glue"
	"github.com/hashcloak/Meson-server/internal/packet"
	"github.com/hashcloak/Meson-server/internal/pkicache"
	"github.com/hashcloak/Meson-server/internal/provider/kaetzchen"
	"github.com/katzenpost/core/constants"
	"github.com/katzenpost/core/crypto/rand"
	"github.com/katzenpost/core/monotime"
	"github.com/katzenpost/core/pki"
	"github.com/katzenpost/core/sphinx"
	"github.com/katzenpost/core/sphinx/commands"
	sConstants "github.com/katzenpost/core/sphinx/constants"
	"github.com/katzenpost/core/sphinx/path"
	"github.com/katzenpost/core/worker"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/op/go-logging.v1"
)

const maxAttempts = 3

var errMaxAttempts = errors.New("decoy: max path selection attempts exceeded")

type surbCtx struct {
	id      uint64
	eta     time.Duration
	sprpKey []byte

	etaNode *avl.Node
}

type decoy struct {
	worker.Worker
	sync.Mutex

	glue glue.Glue
	log  *logging.Logger

	recipient []byte
	rng       *mRand.Rand
	docCh     chan *pkicache.Entry

	surbETAs   *avl.Tree
	surbStore  map[uint64]*surbCtx
	surbIDBase uint64
}

// Prometheus metrics
var (
	packetsDropped = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: internalConstants.Namespace,
			Name:      "dropped_packets_total",
			Subsystem: internalConstants.DecoySubsystem,
			Help:      "Number of dropped packets",
		},
	)
	ignoredPKIDocs = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: internalConstants.Namespace,
			Name:      "documents_ignored_total",
			Subsystem: internalConstants.DecoySubsystem,
			Help:      "Number of ignored PKI Documents",
		},
	)
	pkiDocs = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: internalConstants.Namespace,
			Name:      "pki_docs_per_epoch_total",
			Subsystem: internalConstants.DecoySubsystem,
			Help:      "Number of pki docs in an epoch",
		},
		[]string{"epoch"},
	)
)

func initPrometheus() {
	prometheus.MustRegister(packetsDropped)
	prometheus.MustRegister(ignoredPKIDocs)
	prometheus.MustRegister(pkiDocs)
}

func (d *decoy) OnNewDocument(ent *pkicache.Entry) {
	d.docCh <- ent
}

func (d *decoy) OnPacket(pkt *packet.Packet) {
	// Note: This is called from the crypto worker context, which is "fine".
	defer pkt.Dispose()

	if !pkt.IsSURBReply() {
		panic("BUG: OnPacket called with non-SURB Reply")
	}

	// Ensure that the SURB Reply is destined for the correct recipient,
	// and that it was generated by this decoy instance.  Note that neither
	// fields are visible to any other party involved.
	if subtle.ConstantTimeCompare(pkt.Recipient.ID[:], d.recipient) != 1 {
		d.log.Debugf("Dropping packet: %v (Invalid recipient)", pkt.ID)
		packetsDropped.Inc()
		return
	}

	idBase, id := binary.BigEndian.Uint64(pkt.SurbReply.ID[0:]), binary.BigEndian.Uint64(pkt.SurbReply.ID[8:])
	if idBase != d.surbIDBase {
		d.log.Debugf("Dropping packet: %v (Invalid SURB ID base: %v)", pkt.ID, idBase)
		packetsDropped.Inc()
		return
	}

	d.log.Debugf("Response packet: %v", pkt.ID)

	ctx := d.loadAndDeleteSURBCtx(id)
	if ctx == nil {
		d.log.Debugf("Dropping packet: %v (Unknown SURB ID: 0x%08x)", pkt.ID, id)
		packetsDropped.Inc()
		return
	}

	if _, err := sphinx.DecryptSURBPayload(pkt.Payload, ctx.sprpKey); err != nil {
		d.log.Debugf("Dropping packet: %v (SURB ID: 0x08x%): %v", pkt.ID, id, err)
		packetsDropped.Inc()
		return
	}

	// TODO: At some point, this should do more than just log.
	d.log.Debugf("Response packet: %v (SURB ID: 0x%08x): ETA: %v, Actual: %v (DeltaT: %v)", pkt.ID, id, ctx.eta, pkt.RecvAt, pkt.RecvAt-ctx.eta)
}

func (d *decoy) worker() {
	// Initialize prometheus metrics
	initPrometheus()

	const maxDuration = math.MaxInt64

	wakeInterval := time.Duration(maxDuration)
	timer := time.NewTimer(wakeInterval)
	defer timer.Stop()

	var docCache *pkicache.Entry
	for {
		var timerFired bool
		select {
		case <-d.HaltCh():
			d.log.Debugf("Terminating gracefully.")
			return
		case newEnt := <-d.docCh:
			if !d.glue.Config().Debug.SendDecoyTraffic {
				d.log.Debugf("Received PKI document but decoy traffic is disabled, ignoring.")
				ignoredPKIDocs.Inc()
				continue
			}

			now, _, _, err := d.glue.PKI().Now()
			if err != nil {
				d.log.Debugf("Error fetching PKI epoch, ignoring: %v", newEnt.Epoch())
				ignoredPKIDocs.Inc()
				continue
			}
			if entEpoch := newEnt.Epoch(); entEpoch != now {
				d.log.Debugf("Received PKI document for non-current epoch, ignoring: %v", entEpoch)
				ignoredPKIDocs.Inc()
				continue
			}
			if d.glue.Config().Server.IsProvider {
				d.log.Debugf("Received PKI document when Provider, ignoring (not supported yet).")
				ignoredPKIDocs.Inc()
				continue
			}
			d.log.Debugf("Received new PKI document for epoch: %v", now)
			pkiDocs.With(prometheus.Labels{"epoch": fmt.Sprintf("%v", now)}).Inc()
			docCache = newEnt
		case <-timer.C:
			timerFired = true
		}

		now, _, _, err := d.glue.PKI().Now()
		if err != nil || docCache == nil || docCache.Epoch() != now {
			d.log.Debugf("Suspending operation till the next PKI document.")
			wakeInterval = time.Duration(maxDuration)
		} else {
			// The timer fired, and there is a valid document for this epoch.
			if timerFired {
				d.sendDecoyPacket(docCache)
			}

			// Schedule the next decoy packet.
			//
			// This closely follows how the mailproxy worker schedules
			// outgoing sends, except that the SendShift value is ignored.
			//
			// TODO: Eventually this should use separate parameters.
			doc := docCache.Document()
			wakeMsec := uint64(rand.Exp(d.rng, doc.LambdaM))
			if wakeMsec > doc.LambdaMMaxDelay {
				wakeMsec = doc.LambdaMMaxDelay
			}
			wakeInterval = time.Duration(wakeMsec) * time.Millisecond
			d.log.Debugf("Next wakeInterval: %v", wakeInterval)

			d.sweepSURBCtxs()
		}
		if !timerFired && !timer.Stop() {
			<-timer.C
		}
		timer.Reset(wakeInterval)
	}
}

func (d *decoy) sendDecoyPacket(ent *pkicache.Entry) {
	// TODO: (#52) Do nothing if the rate limiter would discard the packet(?).

	// TODO: Determine if this should be a loop or discard packet.
	isLoopPkt := true // HACK HACK HACK HACK.

	selfDesc := ent.Self()
	if selfDesc.Layer == pki.LayerProvider {
		// The code doesn't handle this correctly yet.  It does need to
		// happen eventually though.
		panic("BUG: Provider generated decoy traffic not supported yet")
	}
	doc := ent.Document()

	// TODO: The path selection maybe should be more strategic/systematic
	// rather than randomized, but this is obviously correct and leak proof.

	// Find a random Provider that is running a loop/discard service.
	var providerDesc *pki.MixDescriptor
	var loopRecip string
	for _, idx := range d.rng.Perm(len(doc.Providers)) {
		desc := doc.Providers[idx]
		params, ok := desc.Kaetzchen[kaetzchen.LoopCapability]
		if !ok {
			continue
		}
		loopRecip, ok = params["endpoint"].(string)
		if !ok {
			continue
		}
		providerDesc = desc
		break
	}
	if providerDesc == nil {
		d.log.Debugf("Failed to find suitable provider")
		return
	}

	if isLoopPkt {
		d.sendLoopPacket(doc, []byte(loopRecip), selfDesc, providerDesc)
		return
	}
	d.sendDiscardPacket(doc, []byte(loopRecip), selfDesc, providerDesc)
}

func (d *decoy) sendLoopPacket(doc *pki.Document, recipient []byte, src, dst *pki.MixDescriptor) {
	var surbID [sConstants.SURBIDLength]byte
	d.makeSURBID(&surbID)

	for attempts := 0; attempts < maxAttempts; attempts++ {
		now := time.Now()

		fwdPath, then, err := path.New(d.rng, doc, recipient, src, dst, &surbID, time.Now(), false, true)
		if err != nil {
			d.log.Debugf("Failed to select forward path: %v", err)
			return
		}

		revPath, then, err := path.New(d.rng, doc, d.recipient, dst, src, &surbID, then, false, false)
		if err != nil {
			d.log.Debugf("Failed to select reverse path: %v", err)
			return
		}

		if deltaT := then.Sub(now); deltaT < epochtime.TestPeriod*2 {
			var zeroBytes [constants.UserForwardPayloadLength]byte
			payload := make([]byte, 2, 2+sphinx.SURBLength+constants.UserForwardPayloadLength)
			payload[0] = 1 // Packet has a SURB.

			surb, k, err := sphinx.NewSURB(rand.Reader, revPath)
			if err != nil {
				d.log.Debugf("Failed to generate SURB: %v", err)
			}
			payload = append(payload, surb...)
			payload = append(payload, zeroBytes[:]...)

			// TODO: This should probably also store path information,
			// so that it's possible to figure out which links/nodes
			// are causing issues.
			ctx := &surbCtx{
				id:      binary.BigEndian.Uint64(surbID[8:]),
				eta:     monotime.Now() + deltaT,
				sprpKey: k,
			}
			d.storeSURBCtx(ctx)

			pkt, err := sphinx.NewPacket(rand.Reader, fwdPath, payload)
			if err != nil {
				d.log.Debugf("Failed to generate Sphinx packet: %v", err)
				return
			}

			_ = d.logPath(doc, fwdPath)
			_ = d.logPath(doc, revPath)
			d.log.Debugf("Dispatching loop packet: SURB ID: 0x%08x", binary.BigEndian.Uint64(surbID[8:]))

			d.dispatchPacket(fwdPath, pkt)
			return
		}
	}

	d.log.Debugf("Failed to generate loop packet: %v", errMaxAttempts)
}

func (d *decoy) sendDiscardPacket(doc *pki.Document, recipient []byte, src, dst *pki.MixDescriptor) {
	var payload [2 + sphinx.SURBLength + constants.UserForwardPayloadLength]byte

	for attempts := 0; attempts < maxAttempts; attempts++ {
		now := time.Now()

		fwdPath, then, err := path.New(d.rng, doc, recipient, src, dst, nil, time.Now(), false, true)
		if err != nil {
			d.log.Debugf("Failed to select forward path: %v", err)
			return
		}

		if then.Sub(now) < epochtime.TestPeriod*2 {
			pkt, err := sphinx.NewPacket(rand.Reader, fwdPath, payload[:])
			if err != nil {
				d.log.Debugf("Failed to generate Sphinx packet: %v", err)
				return
			}
			_ = d.logPath(doc, fwdPath)
			d.dispatchPacket(fwdPath, pkt)
			return
		}
	}

	d.log.Debugf("Failed to generate discard decoy packet: %v", errMaxAttempts)
}

func (d *decoy) dispatchPacket(fwdPath []*sphinx.PathHop, raw []byte) {
	pkt, err := packet.New(raw)
	if err != nil {
		d.log.Debugf("Failed to allocate packet: %v", err)
		return
	}
	pkt.NextNodeHop = &commands.NextNodeHop{}
	copy(pkt.NextNodeHop.ID[:], fwdPath[0].ID[:])
	pkt.DispatchAt = monotime.Now()

	d.log.Debugf("Dispatching packet: %v", pkt.ID)
	d.glue.Connector().DispatchPacket(pkt)
}

func (d *decoy) makeSURBID(surbID *[sConstants.SURBIDLength]byte) {
	// Generate a random SURB ID, prefixed with the time that the decoy
	// instance was initialized.

	binary.BigEndian.PutUint64(surbID[0:], d.surbIDBase)
	binary.BigEndian.PutUint64(surbID[8:], d.rng.Uint64())
}

func (d *decoy) logPath(doc *pki.Document, p []*sphinx.PathHop) error {
	s, err := path.ToString(doc, p)
	if err != nil {
		return err
	}

	for _, v := range s {
		d.log.Debug(v)
	}
	return nil
}

func (d *decoy) storeSURBCtx(ctx *surbCtx) {
	d.Lock()
	defer d.Unlock()

	ctxList := []*surbCtx{ctx}
	ctx.etaNode = d.surbETAs.Insert(ctxList)
	if nCtxList := ctx.etaNode.Value.([]*surbCtx); nCtxList[0] != ctx {
		// Existing node, append the context to the list stored
		// as the node value.  This should be exceedingly rare.
		ctx.etaNode.Value = append(nCtxList, ctx)
	}

	d.surbStore[ctx.id] = ctx
}

func (d *decoy) loadAndDeleteSURBCtx(id uint64) *surbCtx {
	d.Lock()
	defer d.Unlock()

	ctx := d.surbStore[id]
	if ctx == nil {
		return nil
	}
	delete(d.surbStore, id)

	nCtxList := ctx.etaNode.Value.([]*surbCtx)
	if l := len(nCtxList); l > 1 {
		// There is more than 1 SURB with this ETA, remove the context from
		// the list, and leave the node in the tree.
		for i, v := range nCtxList {
			if v == ctx {
				copy(nCtxList[i:], nCtxList[i+1:])
				nCtxList[l-1] = nil
				ctx.etaNode.Value = nCtxList[l-1]
				ctx.etaNode = nil
				return ctx
			}
		}
		panic("BUG: SURB missing from node ETA list")
	} else if nCtxList[0] != ctx {
		panic("BUG: SURB mismatch in node ETA list")
	}

	d.surbETAs.Remove(ctx.etaNode)
	ctx.etaNode = nil

	return ctx
}

func (d *decoy) sweepSURBCtxs() {
	d.Lock()
	defer d.Unlock()

	if d.surbETAs.Len() == 0 {
		d.log.Debugf("Sweep: No outstanding SURBs.")
		return
	}

	now := monotime.Now()
	slack := time.Duration(d.glue.Config().Debug.DecoySlack) * time.Millisecond

	var swept int
	iter := d.surbETAs.Iterator(avl.Forward)
	for node := iter.First(); node != nil; node = iter.Next() {
		surbCtxs := node.Value.([]*surbCtx)
		if surbCtxs[0].eta+slack > now {
			break
		}

		for _, ctx := range surbCtxs {
			delete(d.surbStore, ctx.id)
			// TODO: At some point, this should do more than just log.
			d.log.Debugf("Sweep: Lost SURB ID: 0x%08x ETA: %v (DeltaT: %v)", ctx.id, ctx.eta, now-ctx.eta)
			swept++
		}
		d.surbETAs.Remove(node)
	}

	d.log.Debugf("Sweep: Count: %v (Removed: %v, Elapsed: %v)", len(d.surbStore), swept, monotime.Now()-now)
}

// New constructs a new decoy instance.
func New(glue glue.Glue) (glue.Decoy, error) {
	d := &decoy{
		glue:      glue,
		log:       glue.LogBackend().GetLogger("decoy"),
		recipient: make([]byte, sConstants.RecipientIDLength),
		rng:       rand.NewMath(),
		docCh:     make(chan *pkicache.Entry),
		surbETAs: avl.New(func(a, b interface{}) int {
			surbCtxsA, surbCtxsB := a.([]*surbCtx), b.([]*surbCtx)
			etaA, etaB := surbCtxsA[0].eta, surbCtxsB[0].eta
			switch {
			case etaA < etaB:
				return -1
			case etaA > etaB:
				return 1
			default:
				return 0
			}
		}),
		surbStore:  make(map[uint64]*surbCtx),
		surbIDBase: uint64(time.Now().Unix()),
	}
	if _, err := io.ReadFull(rand.Reader, d.recipient); err != nil {
		return nil, err
	}

	d.Go(d.worker)
	return d, nil
}
