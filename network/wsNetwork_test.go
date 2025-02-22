// Copyright (C) 2019 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package network

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-deadlock"
	"github.com/algorand/websocket"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/metrics"
)

const sendBufferLength = 1000

func TestMain(m *testing.M) {
	logging.Base().SetLevel(logging.Debug)
	os.Exit(m.Run())
}

func debugMetrics(t *testing.T) {
	var buf strings.Builder
	metrics.DefaultRegistry().WriteMetrics(&buf, "")
	t.Log(buf.String())
}

type emptyPhonebook struct{}

func (e *emptyPhonebook) GetAddresses(n int) []string {
	return []string{}
}

var emptyPhonebookSingleton = &emptyPhonebook{}

type oneEntryPhonebook struct {
	Entry string
}

func (e *oneEntryPhonebook) GetAddresses(n int) []string {
	return []string{e.Entry}
}

var defaultConfig config.Local

func init() {
	defaultConfig = config.GetDefaultLocal()
	defaultConfig.Archival = false
	defaultConfig.GossipFanout = 4
	defaultConfig.NetAddress = "127.0.0.1:0"
	defaultConfig.BaseLoggerDebugLevel = uint32(logging.Debug)
	defaultConfig.IncomingConnectionsLimit = -1
	defaultConfig.DNSBootstrapID = ""
	defaultConfig.MaxConnectionsPerIP = 30
}

func makeTestWebsocketNodeWithConfig(t testing.TB, conf config.Local) *WebsocketNetwork {
	log := logging.TestingLog(t)
	log.SetLevel(logging.Level(conf.BaseLoggerDebugLevel))
	wn := &WebsocketNetwork{
		log:       log,
		config:    conf,
		phonebook: emptyPhonebookSingleton,
		GenesisID: "go-test-network-genesis",
		NetworkID: config.Devtestnet,
	}
	wn.setup()
	wn.eventualReadyDelay = time.Second
	return wn
}

func makeTestWebsocketNode(t testing.TB) *WebsocketNetwork {
	return makeTestWebsocketNodeWithConfig(t, defaultConfig)
}

type messageCounterHandler struct {
	target  int
	limit   int
	count   int
	lock    deadlock.Mutex
	done    chan struct{}
	t       testing.TB
	action  ForwardingPolicy
	verbose bool

	// For deterministically simulating slow handlers, block until test code says to go.
	release    sync.Cond
	shouldWait int32
	waitcount  int
}

func (mch *messageCounterHandler) Handle(message IncomingMessage) OutgoingMessage {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	if mch.verbose && len(message.Data) == 8 {
		now := time.Now().UnixNano()
		sent := int64(binary.LittleEndian.Uint64(message.Data))
		dnanos := now - sent
		mch.t.Logf("msg trans time %dns", dnanos)
	}
	if atomic.LoadInt32(&mch.shouldWait) > 0 {
		mch.waitcount++
		mch.release.Wait()
		mch.waitcount--
	}
	mch.count++
	//mch.t.Logf("msg %d %#v", mch.count, message)
	if mch.target != 0 && mch.done != nil && mch.count >= mch.target {
		//mch.t.Log("mch target")
		close(mch.done)
		mch.done = nil
	}
	if mch.limit > 0 && mch.done != nil && mch.count > mch.limit {
		close(mch.done)
		mch.done = nil
	}
	return OutgoingMessage{Action: mch.action}
}

func (mch *messageCounterHandler) numWaiters() int {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	return mch.waitcount
}
func (mch *messageCounterHandler) Count() int {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	return mch.count
}
func (mch *messageCounterHandler) Signal() {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	mch.release.Signal()
}
func (mch *messageCounterHandler) Broadcast() {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	mch.release.Broadcast()
}

func newMessageCounter(t testing.TB, target int) *messageCounterHandler {
	return &messageCounterHandler{target: target, done: make(chan struct{}), t: t}
}

const debugTag = protocol.Tag("DD")

func TestWebsocketNetworkStartStop(t *testing.T) {
	netA := makeTestWebsocketNode(t)
	netA.Start()
	netA.Stop()
}

func waitReady(t testing.TB, wn *WebsocketNetwork, timeout <-chan time.Time) bool {
	select {
	case <-wn.Ready():
		return true
	case <-timeout:
		_, file, line, _ := runtime.Caller(1)
		t.Fatalf("%s:%d timeout waiting for ready", file, line)
		return false
	}
}

// Set up two nodes, test that a.Broadcast is received by B
func TestWebsocketNetworkBasic(t *testing.T) {
	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 2)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	netA.Broadcast(context.Background(), debugTag, []byte("foo"), false, nil)
	netA.Broadcast(context.Background(), debugTag, []byte("bar"), false, nil)

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		t.Errorf("timeout, count=%d, wanted 2", counter.count)
	}
}

// Repeat basic, but test a unicast
func TestWebsocketNetworkUnicast(t *testing.T) {
	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 2)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	require.Equal(t, 1, len(netA.peers))
	require.Equal(t, 1, len(netA.GetPeers(PeersConnectedIn)))
	peerB := netA.peers[0]
	err := peerB.Unicast(context.Background(), []byte("foo"), debugTag)
	assert.NoError(t, err)
	err = peerB.Unicast(context.Background(), []byte("bar"), debugTag)
	assert.NoError(t, err)

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		t.Errorf("timeout, count=%d, wanted 2", counter.count)
	}
}

// Set up two nodes, test that a.Broadcast is received by B, when B has no address.
func TestWebsocketNetworkNoAddress(t *testing.T) {
	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	noAddressConfig := defaultConfig
	noAddressConfig.NetAddress = ""
	netB := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 2)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	netA.Broadcast(context.Background(), debugTag, []byte("foo"), false, nil)
	netA.Broadcast(context.Background(), debugTag, []byte("bar"), false, nil)

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		t.Errorf("timeout, count=%d, wanted 2", counter.count)
	}
}

func lineNetwork(t *testing.T, numNodes int) (nodes []*WebsocketNetwork, counters []messageCounterHandler) {
	nodes = make([]*WebsocketNetwork, numNodes)
	counters = make([]messageCounterHandler, numNodes)
	for i := range nodes {
		nodes[i] = makeTestWebsocketNode(t)
		nodes[i].log = nodes[i].log.With("node", i)
		nodes[i].config.GossipFanout = 2
		if i == 0 || i == len(nodes)-1 {
			nodes[i].config.GossipFanout = 1
		}
		if i > 0 {
			addrPrev, postListen := nodes[i-1].Address()
			require.True(t, postListen)
			nodes[i].phonebook = &oneEntryPhonebook{addrPrev}
			nodes[i].RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: &counters[i]}})
		}
		nodes[i].Start()
		counters[i].t = t
		counters[i].action = Broadcast
	}
	return
}

func closeNodeWG(node *WebsocketNetwork, wg *sync.WaitGroup) {
	node.Stop()
	wg.Done()
}

func closeNodes(nodes []*WebsocketNetwork) {
	wg := sync.WaitGroup{}
	wg.Add(len(nodes))
	for _, node := range nodes {
		go closeNodeWG(node, &wg)
	}
	wg.Wait()
}

func waitNodesReady(t *testing.T, nodes []*WebsocketNetwork, timeout time.Duration) {
	tc := time.After(timeout)
	for i, node := range nodes {
		select {
		case <-node.Ready():
		case <-tc:
			t.Fatalf("node[%d] not ready at timeout", i)
		}
	}
}

const lineNetworkLength = 20
const lineNetworkNumMessages = 5

// Set up a network where each node connects to the previous; test that .Broadcast from one end gets to the other.
// Bonus! Measure how long that takes.
// TODO: also make a Benchmark version of this that reports per-node broadcast hop speed.
func TestLineNetwork(t *testing.T) {
	nodes, counters := lineNetwork(t, lineNetworkLength)
	t.Logf("line network length: %d", lineNetworkLength)
	waitNodesReady(t, nodes, 2*time.Second)
	t.Log("ready")
	defer closeNodes(nodes)
	counter := &counters[len(counters)-1]
	counter.target = lineNetworkNumMessages
	counter.done = make(chan struct{})
	counterDone := counter.done
	counter.verbose = true
	for i := 0; i < lineNetworkNumMessages; i++ {
		sendTime := time.Now().UnixNano()
		var timeblob [8]byte
		binary.LittleEndian.PutUint64(timeblob[:], uint64(sendTime))
		nodes[0].Broadcast(context.Background(), debugTag, timeblob[:], true, nil)
	}
	select {
	case <-counterDone:
	case <-time.After(20 * time.Second):
		t.Errorf("timeout, count=%d, wanted %d", counter.Count(), lineNetworkNumMessages)
		for ci := range counters {
			t.Errorf("count[%d]=%d", ci, counters[ci].Count())
		}
	}
	debugMetrics(t)
}

func addrtest(t *testing.T, wn *WebsocketNetwork, expected, src string) {
	actual, err := wn.addrToGossipAddr(src)
	require.NoError(t, err)
	assert.Equal(t, expected, actual)
}

func TestAddrToGossipAddr(t *testing.T) {
	wn := &WebsocketNetwork{}
	wn.GenesisID = "test genesisID"
	wn.log = logging.Base()
	addrtest(t, wn, "ws://r7.algodev.network.:4166/v1/test%20genesisID/gossip", "r7.algodev.network.:4166")
	addrtest(t, wn, "ws://r7.algodev.network.:4166/v1/test%20genesisID/gossip", "http://r7.algodev.network.:4166")
	addrtest(t, wn, "wss://r7.algodev.network.:4166/v1/test%20genesisID/gossip", "https://r7.algodev.network.:4166")
}

type nopConn struct{}

func (nc *nopConn) RemoteAddr() net.Addr {
	return nil
}
func (nc *nopConn) NextReader() (int, io.Reader, error) {
	return 0, nil, nil
}
func (nc *nopConn) WriteMessage(int, []byte) error {
	return nil
}
func (nc *nopConn) SetReadLimit(limit int64) {
}
func (nc *nopConn) CloseWithoutFlush() error {
	return nil
}

var nopConnSingleton = nopConn{}

// What happens when all the read message handler threads get busy?
func TestSlowHandlers(t *testing.T) {
	slowTag := protocol.Tag("sl")
	fastTag := protocol.Tag("fa")
	slowCounter := messageCounterHandler{shouldWait: 1}
	slowCounter.release.L = &slowCounter.lock
	fastCounter := messageCounterHandler{target: incomingThreads}
	fastCounter.done = make(chan struct{})
	fastCounterDone := fastCounter.done
	slowHandler := TaggedMessageHandler{Tag: slowTag, MessageHandler: &slowCounter}
	fastHandler := TaggedMessageHandler{Tag: fastTag, MessageHandler: &fastCounter}
	node := makeTestWebsocketNode(t)
	node.RegisterHandlers([]TaggedMessageHandler{slowHandler, fastHandler})
	node.Start()
	defer node.Stop()
	injectionPeers := make([]wsPeer, incomingThreads*2)
	for i := range injectionPeers {
		injectionPeers[i].closing = make(chan struct{})
		injectionPeers[i].net = node
		injectionPeers[i].conn = &nopConnSingleton
		node.addPeer(&injectionPeers[i])
	}
	ipi := 0
	// start slow handler calls that will block all handler threads
	for i := 0; i < incomingThreads; i++ {
		data := []byte{byte(i)}
		node.readBuffer <- IncomingMessage{Sender: &injectionPeers[ipi], Tag: slowTag, Data: data, Net: node}
		ipi++
	}
	defer slowCounter.Broadcast()

	// start fast handler calls that won't get to run
	for i := 0; i < incomingThreads; i++ {
		data := []byte{byte(i)}
		node.readBuffer <- IncomingMessage{Sender: &injectionPeers[ipi], Tag: fastTag, Data: data, Net: node}
		ipi++
	}
	ok := false
	for i := 0; i < 10; i++ {
		time.Sleep(time.Millisecond)
		nw := slowCounter.numWaiters()
		if nw == incomingThreads {
			ok = true
			break
		}
		t.Logf("%dms %d waiting", i+1, nw)
	}
	if !ok {
		t.Errorf("timeout waiting for %d threads to block on slow handler, have %d", incomingThreads, slowCounter.numWaiters())
	}
	require.Equal(t, 0, fastCounter.Count())

	// release one slow request, all the other requests should process on that one handler thread
	slowCounter.Signal()

	select {
	case <-fastCounterDone:
	case <-time.After(time.Second):
		t.Errorf("timeout waiting for %d blocked events to be handled, have %d", incomingThreads, fastCounter.Count())
	}
	// checks that above .Signal() did in fact release just one waiting slow handler
	require.Equal(t, 1, slowCounter.Count())

	// we don't care about counting how things finish
	debugMetrics(t)
}

// one peer sends waaaayy too much slow-to-handle traffic. everything else should run fine.
func TestFloodingPeer(t *testing.T) {
	t.Skip("flaky test")
	slowTag := protocol.Tag("sl")
	fastTag := protocol.Tag("fa")
	slowCounter := messageCounterHandler{shouldWait: 1}
	slowCounter.release.L = &slowCounter.lock
	fastCounter := messageCounterHandler{}
	slowHandler := TaggedMessageHandler{Tag: slowTag, MessageHandler: &slowCounter}
	fastHandler := TaggedMessageHandler{Tag: fastTag, MessageHandler: &fastCounter}
	node := makeTestWebsocketNode(t)
	node.RegisterHandlers([]TaggedMessageHandler{slowHandler, fastHandler})
	node.Start()
	defer node.Stop()
	injectionPeers := make([]wsPeer, incomingThreads*2)
	for i := range injectionPeers {
		injectionPeers[i].closing = make(chan struct{})
		injectionPeers[i].net = node
		injectionPeers[i].conn = &nopConnSingleton
		node.addPeer(&injectionPeers[i])
	}
	ipi := 0
	const numBadPeers = 1
	// start slow handler calls that will block some threads
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < numBadPeers; i++ {
		myI := i
		myIpi := ipi
		go func() {
			processed := make(chan struct{}, 1)
			processed <- struct{}{}

			for qi := 0; qi < incomingThreads*2; qi++ {
				data := []byte{byte(myI), byte(qi)}
				select {
				case <-processed:
				case <-ctx.Done():
					return
				}

				select {
				case node.readBuffer <- IncomingMessage{Sender: &injectionPeers[myIpi], Tag: slowTag, Data: data, Net: node, processing: processed}:
				case <-ctx.Done():
					return
				}
			}
		}()
		ipi++
	}
	defer cancel()
	defer func() {
		t.Log("release slow handlers")
		atomic.StoreInt32(&slowCounter.shouldWait, 0)
		slowCounter.Broadcast()
	}()

	// start fast handler calls that will run on other reader threads
	numFast := 0
	fastCounter.target = len(injectionPeers) - ipi
	fastCounter.done = make(chan struct{})
	fastCounterDone := fastCounter.done
	for ipi < len(injectionPeers) {
		data := []byte{byte(ipi)}
		node.readBuffer <- IncomingMessage{Sender: &injectionPeers[ipi], Tag: fastTag, Data: data, Net: node}
		numFast++
		ipi++
	}
	require.Equal(t, numFast, fastCounter.target)
	select {
	case <-fastCounterDone:
	case <-time.After(time.Second):
		t.Errorf("timeout waiting for %d fast handlers, got %d", fastCounter.target, fastCounter.Count())
	}

	// we don't care about counting how things finish
}

func peerIsClosed(peer *wsPeer) bool {
	return atomic.LoadInt32(&peer.didInnerClose) != 0
}

func avgSendBufferHighPrioLength(wn *WebsocketNetwork) float64 {
	wn.peersLock.Lock()
	defer wn.peersLock.Unlock()
	sum := 0
	for _, peer := range wn.peers {
		sum += len(peer.sendBufferHighPrio)
	}
	return float64(sum) / float64(len(wn.peers))
}

// TestSlowOutboundPeer tests what happens when one outbound peer is slow and the rest are fine. Current logic is to disconnect the one slow peer when its outbound channel is full.
//
// This is a deeply invasive test that reaches into the guts of WebsocketNetwork and wsPeer. If the implementation chainges consider throwing away or totally reimplementing this test.
func TestSlowOutboundPeer(t *testing.T) {
	t.Skip() // todo - update this test to reflect the new implementation.
	xtag := protocol.ProposalPayloadTag
	node := makeTestWebsocketNode(t)
	destPeers := make([]wsPeer, 5)
	for i := range destPeers {
		destPeers[i].closing = make(chan struct{})
		destPeers[i].net = node
		destPeers[i].sendBufferHighPrio = make(chan sendMessage, sendBufferLength)
		destPeers[i].sendBufferBulk = make(chan sendMessage, sendBufferLength)
		destPeers[i].conn = &nopConnSingleton
		destPeers[i].rootURL = fmt.Sprintf("fake %d", i)
		node.addPeer(&destPeers[i])
	}
	node.Start()
	tctx, cf := context.WithTimeout(context.Background(), 5*time.Second)
	for i := 0; i < sendBufferLength; i++ {
		t.Logf("broadcast %d", i)
		sent := node.Broadcast(tctx, xtag, []byte{byte(i)}, true, nil)
		require.NoError(t, sent)
	}
	cf()
	ok := false
	for i := 0; i < 10; i++ {
		time.Sleep(time.Millisecond)
		aoql := avgSendBufferHighPrioLength(node)
		if aoql == sendBufferLength {
			ok = true
			break
		}
		t.Logf("node.avgOutboundQueueLength() %f", aoql)
	}
	require.True(t, ok)
	for p := range destPeers {
		if p == 0 {
			continue
		}
		for j := 0; j < sendBufferLength; j++ {
			// throw away a message as if sent
			<-destPeers[p].sendBufferHighPrio
		}
	}
	aoql := avgSendBufferHighPrioLength(node)
	if aoql > (sendBufferLength / 2) {
		t.Fatalf("avgOutboundQueueLength=%f wanted <%f", aoql, sendBufferLength/2.0)
		return
	}
	// it shouldn't have closed for just sitting on the limit of full
	require.False(t, peerIsClosed(&destPeers[0]))

	// function context just to contain defer cf()
	func() {
		timeout, cf := context.WithTimeout(context.Background(), time.Second)
		defer cf()
		sent := node.Broadcast(timeout, xtag, []byte{byte(42)}, true, nil)
		assert.NoError(t, sent)
	}()

	// and now with the rest of the peers well and this one slow, we closed the slow one
	require.True(t, peerIsClosed(&destPeers[0]))
}

func makeTestFilterWebsocketNode(t *testing.T, nodename string) *WebsocketNetwork {
	dc := defaultConfig
	dc.EnableIncomingMessageFilter = true
	dc.EnableOutgoingNetworkMessageFiltering = true
	dc.IncomingMessageFilterBucketCount = 5
	dc.IncomingMessageFilterBucketSize = 512
	dc.OutgoingMessageFilterBucketCount = 3
	dc.OutgoingMessageFilterBucketSize = 128
	wn := &WebsocketNetwork{
		log:       logging.TestingLog(t).With("node", nodename),
		config:    dc,
		phonebook: emptyPhonebookSingleton,
		GenesisID: "go-test-network-genesis",
		NetworkID: config.Devtestnet,
	}
	require.True(t, wn.config.EnableIncomingMessageFilter)
	wn.setup()
	wn.eventualReadyDelay = time.Second
	require.True(t, wn.config.EnableIncomingMessageFilter)
	return wn
}

func TestDupFilter(t *testing.T) {
	netA := makeTestFilterWebsocketNode(t, "a")
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestFilterWebsocketNode(t, "b")
	netB.config.GossipFanout = 2
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := &messageCounterHandler{t: t, limit: 1, done: make(chan struct{})}
	netB.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: protocol.AgreementVoteTag, MessageHandler: counter}})
	debugTag2 := protocol.Tag("D2")
	counter2 := &messageCounterHandler{t: t, limit: 1, done: make(chan struct{})}
	netB.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag2, MessageHandler: counter2}})

	addrB, postListen := netB.Address()
	require.True(t, postListen)
	netC := makeTestFilterWebsocketNode(t, "c")
	netC.config.GossipFanout = 1
	netC.phonebook = &oneEntryPhonebook{addrB}
	netC.Start()
	defer netC.Stop()

	msg := make([]byte, messageFilterSize+1)
	rand.Read(msg)

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")
	waitReady(t, netC, readyTimeout.C)
	t.Log("c ready")

	// TODO: this test has two halves that exercise inbound de-dup and outbound non-send due to recieved hash. But it doesn't properly _test_ them as it doesn't measure _why_ it receives each message exactly once. The second half below could actualy be because of the same inbound de-dup as this first half. You can see the actions of either in metrics.
	// algod_network_duplicate_message_received_total{} 2
	// algod_outgoing_network_message_filtered_out_total{} 2
	// Maybe we should just .Set(0) those counters and use them in this test?

	// This exercizes inbound dup detection.
	netA.Broadcast(context.Background(), protocol.AgreementVoteTag, msg, true, nil)
	netA.Broadcast(context.Background(), protocol.AgreementVoteTag, msg, true, nil)
	netA.Broadcast(context.Background(), protocol.AgreementVoteTag, msg, true, nil)
	t.Log("A dup send done")

	select {
	case <-counter.done:
		// probably a failure, but let it fall through to the equal check
	case <-time.After(time.Second):
	}
	counter.lock.Lock()
	assert.Equal(t, 1, counter.count)
	counter.lock.Unlock()

	// new message
	rand.Read(msg)
	t.Log("A send, C non-dup-send")
	netA.Broadcast(context.Background(), debugTag2, msg, true, nil)
	// B should broadcast its non-desire to recieve the message again
	time.Sleep(500 * time.Millisecond)

	// C should now not send these
	netC.Broadcast(context.Background(), debugTag2, msg, true, nil)
	netC.Broadcast(context.Background(), debugTag2, msg, true, nil)

	select {
	case <-counter2.done:
		// probably a failure, but let it fall through to the equal check
	case <-time.After(time.Second):
	}
	assert.Equal(t, 1, counter2.count)

	debugMetrics(t)
}

func TestGetPeers(t *testing.T) {
	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer netA.Stop()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	phba := &oneEntryPhonebook{addrA}
	phbMulti := &MultiPhonebook{}
	phbMulti.AddPhonebook(phba)
	netB.phonebook = phbMulti
	netB.Start()
	defer netB.Stop()

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	ph := ArrayPhonebook{[]string{"a", "b", "c"}}
	phbMulti.AddPhonebook(&ph)

	//addrB, _ := netB.Address()

	// A has only an inbound connection from B
	aPeers := netA.GetPeers(PeersConnectedOut)
	assert.Equal(t, 0, len(aPeers))

	// B's connection to A is outgoing
	bPeers := netB.GetPeers(PeersConnectedOut)
	assert.Equal(t, 1, len(bPeers))
	assert.Equal(t, addrA, bPeers[0].(HTTPPeer).GetAddress())

	// B also knows about other peers not connected to
	bPeers = netB.GetPeers(PeersPhonebook)
	assert.Equal(t, 4, len(bPeers))
	peerAddrs := make([]string, len(bPeers))
	for pi, peer := range bPeers {
		peerAddrs[pi] = peer.(HTTPPeer).GetAddress()
	}
	sort.Strings(peerAddrs)
	expectAddrs := []string{addrA, "a", "b", "c"}
	sort.Strings(expectAddrs)
	assert.Equal(t, expectAddrs, peerAddrs)
}

type benchmarkHandler struct {
	returns chan uint64
}

func (bh *benchmarkHandler) Handle(message IncomingMessage) OutgoingMessage {
	i := binary.LittleEndian.Uint64(message.Data)
	bh.returns <- i
	return OutgoingMessage{}
}

// Set up two nodes, test that a.Broadcast is received by B
func BenchmarkWebsocketNetworkBasic(t *testing.B) {
	deadlock.Opts.Disable = true
	const msgSize = 200
	const inflight = 90
	t.Logf("%s %d", t.Name(), t.N)
	t.StopTimer()
	t.ResetTimer()
	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	returns := make(chan uint64, 100)
	bhandler := benchmarkHandler{returns}
	netB.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: &bhandler}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")
	var ireturned uint64

	t.StartTimer()
	timeoutd := (time.Duration(t.N) * 100 * time.Microsecond) + (2 * time.Second)
	timeout := time.After(timeoutd)
	for i := 0; i < t.N; i++ {
		for uint64(i) > ireturned+inflight {
			select {
			case ireturned = <-returns:
			case <-timeout:
				t.Errorf("timeout in send at %d", i)
				return
			}
		}
		msg := make([]byte, msgSize)
		binary.LittleEndian.PutUint64(msg, uint64(i))
		err := netA.Broadcast(context.Background(), debugTag, msg, true, nil)
		if err != nil {
			t.Errorf("error on broadcast: %v", err)
			return
		}
	}
	netA.Broadcast(context.Background(), protocol.Tag("-1"), []byte("derp"), true, nil)
	t.Logf("sent %d", t.N)

	for ireturned < uint64(t.N-1) {
		select {
		case ireturned = <-returns:
		case <-timeout:
			t.Errorf("timeout, count=%d, wanted %d", ireturned, t.N)
			buf := strings.Builder{}
			networkMessageReceivedTotal.WriteMetric(&buf, "")
			networkMessageSentTotal.WriteMetric(&buf, "")
			networkBroadcasts.WriteMetric(&buf, "")
			duplicateNetworkMessageReceivedTotal.WriteMetric(&buf, "")
			outgoingNetworkMessageFilteredOutTotal.WriteMetric(&buf, "")
			networkBroadcastsDropped.WriteMetric(&buf, "")
			t.Errorf(
				"a out queue=%d, metric: %s",
				len(netA.broadcastQueueBulk),
				buf.String(),
			)
			return
		}
	}
	t.StopTimer()
	t.Logf("counter done")
}

// Check that priority is propagated from B to A
func TestWebsocketNetworkPrio(t *testing.T) {
	prioA := netPrioStub{}
	netA := makeTestWebsocketNode(t)
	netA.SetPrioScheme(&prioA)
	netA.config.GossipFanout = 1
	netA.prioResponseChan = make(chan *wsPeer, 10)
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	prioB := netPrioStub{}
	crypto.RandBytes(prioB.addr[:])
	prioB.prio = crypto.RandUint64()
	netB := makeTestWebsocketNode(t)
	netB.SetPrioScheme(&prioB)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	// Wait for response message to propagate from B to A
	select {
	case <-netA.prioResponseChan:
	case <-time.After(time.Second):
		t.Errorf("timeout on netA.prioResponseChan")
	}
	waitReady(t, netA, time.After(time.Second))

	// Peek at A's peers
	netA.peersLock.RLock()
	defer netA.peersLock.RUnlock()
	require.Equal(t, len(netA.peers), 1)

	require.Equal(t, netA.peers[0].prioAddress, prioB.addr)
	require.Equal(t, netA.peers[0].prioWeight, prioB.prio)
}

// Check that priority is propagated from B to A
func TestWebsocketNetworkPrioLimit(t *testing.T) {
	limitConf := defaultConfig
	limitConf.BroadcastConnectionsLimit = 1

	prioA := netPrioStub{}
	netA := makeTestWebsocketNodeWithConfig(t, limitConf)
	netA.SetPrioScheme(&prioA)
	netA.config.GossipFanout = 2
	netA.prioResponseChan = make(chan *wsPeer, 10)
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	addrA, postListen := netA.Address()
	require.True(t, postListen)

	counterB := newMessageCounter(t, 1)
	counterBdone := counterB.done
	prioB := netPrioStub{}
	crypto.RandBytes(prioB.addr[:])
	prioB.prio = 100
	netB := makeTestWebsocketNode(t)
	netB.SetPrioScheme(&prioB)
	netB.config.GossipFanout = 1
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: counterB}})
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	counterC := newMessageCounter(t, 1)
	counterCdone := counterC.done
	prioC := netPrioStub{}
	crypto.RandBytes(prioC.addr[:])
	prioC.prio = 10
	netC := makeTestWebsocketNode(t)
	netC.SetPrioScheme(&prioC)
	netC.config.GossipFanout = 1
	netC.phonebook = &oneEntryPhonebook{addrA}
	netC.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: counterC}})
	netC.Start()
	defer func() { t.Log("stopping C"); netC.Stop(); t.Log("C done") }()

	// Wait for response messages to propagate from B+C to A
	select {
	case <-netA.prioResponseChan:
	case <-time.After(time.Second):
		t.Errorf("timeout on netA.prioResponseChan 1")
	}
	select {
	case <-netA.prioResponseChan:
	case <-time.After(time.Second):
		t.Errorf("timeout on netA.prioResponseChan 2")
	}
	waitReady(t, netA, time.After(time.Second))

	netA.Broadcast(context.Background(), debugTag, nil, true, nil)

	select {
	case <-counterBdone:
	case <-time.After(time.Second):
		t.Errorf("timeout, B did not receive message")
	}

	select {
	case <-counterCdone:
		t.Errorf("C received message")
	case <-time.After(time.Second):
	}
}

// Create many idle connections, to see if we have excessive CPU utilization.
func TestWebsocketNetworkManyIdle(t *testing.T) {
	// This test is meant to be run manually, as:
	//
	//   IDLETEST=x go test -v . -run=ManyIdle -count=1
	//
	// and examining the reported CPU time use.

	if os.Getenv("IDLETEST") == "" {
		t.Skip("Skipping; IDLETEST not set")
	}

	deadlock.Opts.Disable = true

	numClients := 1000
	relayConf := defaultConfig
	relayConf.BaseLoggerDebugLevel = uint32(logging.Error)
	relayConf.MaxConnectionsPerIP = numClients

	relay := makeTestWebsocketNodeWithConfig(t, relayConf)
	relay.config.GossipFanout = numClients
	relay.Start()
	defer relay.Stop()
	relayAddr, postListen := relay.Address()
	require.True(t, postListen)

	clientConf := defaultConfig
	clientConf.BaseLoggerDebugLevel = uint32(logging.Error)
	clientConf.BroadcastConnectionsLimit = 0
	clientConf.NetAddress = ""

	var clients []*WebsocketNetwork
	for i := 0; i < numClients; i++ {
		client := makeTestWebsocketNodeWithConfig(t, clientConf)
		client.config.GossipFanout = 1
		client.phonebook = &oneEntryPhonebook{relayAddr}
		client.Start()
		defer client.Stop()

		clients = append(clients, client)
	}

	readyTimeout := time.NewTimer(30 * time.Second)
	waitReady(t, relay, readyTimeout.C)

	for i := 0; i < numClients; i++ {
		waitReady(t, clients[i], readyTimeout.C)
	}

	var r0, r1 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
	time.Sleep(10 * time.Second)
	syscall.Getrusage(syscall.RUSAGE_SELF, &r1)

	t.Logf("Background CPU use: user %v, system %v\n",
		time.Duration(r1.Utime.Nano()-r0.Utime.Nano()),
		time.Duration(r1.Stime.Nano()-r0.Stime.Nano()))
}

// TODO: test both sides of http-header setting and checking?
// TODO: test request-disconnect-reconnect?
// TODO: test server handling of various malformed clients?
// TODO? disconnect a node in the middle of a line and test that messages _don't_ get through?
// TODO: test self-connect rejection
// TODO: test funcion when some message handler is slow?

func TestWebsocketNetwork_updateUrlHost(t *testing.T) {
	type fields struct {
		listener               net.Listener
		server                 http.Server
		router                 *mux.Router
		scheme                 string
		upgrader               websocket.Upgrader
		config                 config.Local
		log                    logging.Logger
		readBuffer             chan IncomingMessage
		wg                     sync.WaitGroup
		handlers               Multiplexer
		ctx                    context.Context
		ctxCancel              context.CancelFunc
		peersLock              deadlock.RWMutex
		peers                  []*wsPeer
		broadcastQueueHighPrio chan broadcastRequest
		broadcastQueueBulk     chan broadcastRequest
		phonebook              Phonebook
		dnsPhonebook           ThreadsafePhonebook
		GenesisID              string
		NetworkID              protocol.NetworkID
		RandomID               string
		ready                  int32
		readyChan              chan struct{}
		meshUpdateRequests     chan meshRequest
		tryConnectAddrs        map[string]int64
		tryConnectLock         deadlock.Mutex
		incomingMsgFilter      *messageFilter
		eventualReadyDelay     time.Duration
		relayMessages          bool
		prioScheme             NetPrioScheme
		prioTracker            *prioTracker
		prioResponseChan       chan *wsPeer
	}
	type args struct {
		originalAddress string
		host            string
	}
	testFields1 := fields{log: logging.NewLogger()}
	testFields2 := fields{log: logging.NewLogger()}
	testFields3 := fields{log: logging.NewLogger()}
	testFields4 := fields{log: logging.NewLogger()}
	testFields5 := fields{log: logging.NewLogger()}

	tests := []struct {
		name           string
		fields         *fields
		args           args
		wantNewAddress string
		wantErr        bool
	}{
		{name: "test1 ipv4",
			fields: &testFields1,
			args: args{
				originalAddress: "http://[::]:8080/aaa/bbb/ccc",
				host:            "123.20.50.128"},
			wantNewAddress: "http://123.20.50.128:8080/aaa/bbb/ccc",
			wantErr:        false,
		},
		{name: "test2 ipv6",
			fields: &testFields2,
			args: args{
				originalAddress: "http://[::]:80/aaa/bbb/ccc",
				host:            "2601:192:4b40:6a23:2999:acf5:c0f6:47dc"},
			wantNewAddress: "http://[2601:192:4b40:6a23:2999:acf5:c0f6:47dc]:80/aaa/bbb/ccc",
			wantErr:        false,
		},
		{name: "test3 ipv6 -> ipv4",
			fields: &testFields3,
			args: args{
				originalAddress: "http://[2601:192:4b40:6a23:2999:acf5:c0f6:47dc:7334]:80/aaa/bbb/ccc",
				host:            "123.20.50.128"},
			wantNewAddress: "http://123.20.50.128:80/aaa/bbb/ccc",
			wantErr:        false,
		},
		{name: "test4 ipv6 -> ipv4",
			fields: &testFields4,
			args: args{
				originalAddress: "http://[2601:192:4b40:6a23:2999:acf5:c0f6:47dc]:80/aaa/bbb/ccc",
				host:            "123.20.50.128"},
			wantNewAddress: "http://123.20.50.128:80/aaa/bbb/ccc",
			wantErr:        false,
		},
		{name: "test5 parse error",
			fields: &testFields5,
			args: args{
				originalAddress: "http://[2601:192:4b40:6a23:2999:acf5:c0f6:47dc]:80:aaa/bbb/ccc",
				host:            "123.20.50.128"},
			wantNewAddress: "",
			wantErr:        true,
		},
		{name: "test6 invalid host",
			fields: &testFields5,
			args: args{
				originalAddress: "http://[2001:0db8:85a3:0000:0000:8a2e:0370:7334]:80/aaa/bbb/ccc",
				host:            "123.20.50"},
			wantNewAddress: "",
			wantErr:        false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wn := &WebsocketNetwork{
				log: tt.fields.log,
			}
			gotNewAddress, err := wn.updateURLHost(tt.args.originalAddress, net.ParseIP(tt.args.host))
			if (err != nil) != tt.wantErr {
				t.Errorf("WebsocketNetwork.updateUrlHost() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotNewAddress != tt.wantNewAddress {
				t.Errorf("WebsocketNetwork.updateUrlHost() = %v, want %v", gotNewAddress, tt.wantNewAddress)
			}
		})
	}
}

func TestWebsocketNetwork_checkHeaders(t *testing.T) {
	type fields struct {
		listener               net.Listener
		server                 http.Server
		router                 *mux.Router
		scheme                 string
		upgrader               websocket.Upgrader
		config                 config.Local
		log                    logging.Logger
		readBuffer             chan IncomingMessage
		wg                     sync.WaitGroup
		handlers               Multiplexer
		ctx                    context.Context
		ctxCancel              context.CancelFunc
		peersLock              deadlock.RWMutex
		peers                  []*wsPeer
		broadcastQueueHighPrio chan broadcastRequest
		broadcastQueueBulk     chan broadcastRequest
		phonebook              Phonebook
		dnsPhonebook           ThreadsafePhonebook
		GenesisID              string
		NetworkID              protocol.NetworkID
		RandomID               string
		ready                  int32
		readyChan              chan struct{}
		meshUpdateRequests     chan meshRequest
		tryConnectAddrs        map[string]int64
		tryConnectLock         deadlock.Mutex
		incomingMsgFilter      *messageFilter
		eventualReadyDelay     time.Duration
		relayMessages          bool
		prioScheme             NetPrioScheme
		prioTracker            *prioTracker
		prioResponseChan       chan *wsPeer
	}

	const xForwardedAddrHeaderKey = "X-Forwarded-Addr"
	const CFxForwardedAddrHeaderKey = "Cf-Connecting-Ip"
	wn1 := makeTestWebsocketNode(t)
	wn1.config.UseXForwardedForAddressField = xForwardedAddrHeaderKey
	testFields1 := fields{
		log:      wn1.log,
		config:   wn1.config,
		RandomID: wn1.RandomID,
	}
	wn2 := makeTestWebsocketNode(t)
	wn2.config.UseXForwardedForAddressField = xForwardedAddrHeaderKey
	testFields2 := fields{
		log:      wn2.log,
		config:   wn2.config,
		RandomID: wn2.RandomID,
	}
	wn3 := makeTestWebsocketNode(t)
	wn3.config.UseXForwardedForAddressField = CFxForwardedAddrHeaderKey
	testFields3 := fields{
		log:      wn3.log,
		config:   wn3.config,
		RandomID: wn3.RandomID,
	}
	wn4 := makeTestWebsocketNode(t)
	wn4.config.UseXForwardedForAddressField = ""
	testFields4 := fields{
		log:      wn4.log,
		config:   wn4.config,
		RandomID: wn4.RandomID,
	}

	type args struct {
		header http.Header
		addr   string
	}

	tests := []struct {
		name                   string
		fields                 *fields
		args                   args
		wantOk                 bool
		wantOtherTelemetryGUID string
		wantOtherPublicAddr    string
		wantOtherInstanceName  string
	}{
		{name: "test1 ipv4",
			fields: &testFields1,
			args: args{
				header: http.Header{
					ProtocolVersionHeader:   []string{"1"},
					GenesisHeader:           []string{""},
					NodeRandomHeader:        []string{"node random header"},
					TelemetryIDHeader:       []string{"telemetry id header"},
					AddressHeader:           []string{"http://[::]:8080/aaa/bbb/ccc"},
					xForwardedAddrHeaderKey: []string{"12.12.12.12", "1.2.3.4", "55.55.55.55"},
					InstanceNameHeader:      []string{"instance header name"},
				},
				addr: "http://123.20.50.128:8080/aaa/bbb/ccc"},
			wantOk:                 true,
			wantOtherTelemetryGUID: "",
			wantOtherPublicAddr:    "http://12.12.12.12:8080/aaa/bbb/ccc",
			wantOtherInstanceName:  "",
		},
		{name: "test2 ipv6",
			fields: &testFields2,
			args: args{
				header: http.Header{
					ProtocolVersionHeader:   []string{"1"},
					GenesisHeader:           []string{""},
					NodeRandomHeader:        []string{"node random header"},
					TelemetryIDHeader:       []string{"telemetry id header"},
					AddressHeader:           []string{"http://[::]:8080/aaa/bbb/ccc"},
					xForwardedAddrHeaderKey: []string{"2601:192:4b40:6a23:2999:acf5:c0f6:47dc"},
					InstanceNameHeader:      []string{"instance header name"},
				},
				addr: "http://123.20.50.128:8080/aaa/bbb/ccc"},
			wantOk:                 true,
			wantOtherTelemetryGUID: "",
			wantOtherPublicAddr:    "http://[2601:192:4b40:6a23:2999:acf5:c0f6:47dc]:8080/aaa/bbb/ccc",
			wantOtherInstanceName:  "",
		},
		{name: "test2 ipv6 no path",
			fields: &testFields3,
			args: args{
				header: http.Header{
					ProtocolVersionHeader:     []string{"1"},
					GenesisHeader:             []string{""},
					NodeRandomHeader:          []string{"node random header"},
					TelemetryIDHeader:         []string{"telemetry id header"},
					AddressHeader:             []string{"http://[::]:80"},
					CFxForwardedAddrHeaderKey: []string{"2601:192:4b40:6a23:2999:acf5:c0f6:47dc"},
					InstanceNameHeader:        []string{"instance header name"},
				},
				addr: "http://123.20.50.128:8080/aaa/bbb/ccc"},
			wantOk:                 true,
			wantOtherTelemetryGUID: "",
			wantOtherPublicAddr:    "http://[2601:192:4b40:6a23:2999:acf5:c0f6:47dc]:80",
			wantOtherInstanceName:  "",
		},
		{name: "test2 ipv6 no UseXForwardedForAddressField",
			fields: &testFields4,
			args: args{
				header: http.Header{
					ProtocolVersionHeader: []string{"1"},
					GenesisHeader:         []string{""},
					NodeRandomHeader:      []string{"node random header"},
					TelemetryIDHeader:     []string{"telemetry id header"},
					AddressHeader:         []string{"http://[::]:80"},
					InstanceNameHeader:    []string{"instance header name"},
				},
				addr: "http://123.20.50.128:8080/aaa/bbb/ccc"},
			wantOk:                 true,
			wantOtherTelemetryGUID: "",
			wantOtherPublicAddr:    "http://[::]:80",
			wantOtherInstanceName:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wn := &WebsocketNetwork{
				config:    tt.fields.config,
				log:       tt.fields.log,
				GenesisID: tt.fields.GenesisID,
				RandomID:  tt.fields.RandomID,
			}

			t.Logf("headers %+v", tt.args.header)
			gotOk, gotOtherTelemetryGUID, gotOtherPublicAddr, gotOtherInstanceName := wn.checkHeaders(tt.args.header, tt.args.addr, wn.getForwardedConnectionAddress(tt.args.header))
			if gotOk != tt.wantOk {
				t.Errorf("WebsocketNetwork.checkHeaders() gotOk = %v, want %v", gotOk, tt.wantOk)
			}
			if gotOtherTelemetryGUID != tt.wantOtherTelemetryGUID {
				t.Errorf("WebsocketNetwork.checkHeaders() gotOtherTelemetryGUID = %v, want %v", gotOtherTelemetryGUID, tt.wantOtherTelemetryGUID)
			}
			if gotOtherPublicAddr != tt.wantOtherPublicAddr {
				t.Errorf("WebsocketNetwork.checkHeaders() gotOtherPublicAddr = %v, want %v", gotOtherPublicAddr, tt.wantOtherPublicAddr)
			}
			if gotOtherInstanceName != tt.wantOtherInstanceName {
				t.Errorf("WebsocketNetwork.checkHeaders() gotOtherInstanceName = %v, want %v", gotOtherInstanceName, tt.wantOtherInstanceName)
			}
		})
	}
}

func (wn *WebsocketNetwork) broadcastWithTimestamp(tag protocol.Tag, data []byte, when time.Time) error {
	request := broadcastRequest{tag: tag, data: data, enqueueTime: when}

	broadcastQueue := wn.broadcastQueueBulk
	if highPriorityTag(tag) {
		broadcastQueue = wn.broadcastQueueHighPrio
	}
	// no wait
	select {
	case broadcastQueue <- request:
		return nil
	default:
		return errBcastQFull
	}
}

func TestDelayedMessageDrop(t *testing.T) {
	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	noAddressConfig := defaultConfig
	noAddressConfig.NetAddress = ""
	netB := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 5)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)

	currentTime := time.Now()
	for i := 0; i < 10; i++ {
		err := netA.broadcastWithTimestamp(debugTag, []byte("foo"), currentTime.Add(time.Hour*time.Duration(i-5)))
		require.NoErrorf(t, err, "No error was expected")
	}

	select {
	case <-counterDone:
	case <-time.After(maxMessageQueueDuration):
		require.Equalf(t, 5, counter.count, "One or more messages failed to reach destination network")
	}
}

func TestSlowPeerDisconnection(t *testing.T) {
	log := logging.TestingLog(t)
	log.SetLevel(logging.Level(defaultConfig.BaseLoggerDebugLevel))
	wn := &WebsocketNetwork{
		log:                            log,
		config:                         defaultConfig,
		phonebook:                      emptyPhonebookSingleton,
		GenesisID:                      "go-test-network-genesis",
		NetworkID:                      config.Devtestnet,
		slowWritingPeerMonitorInterval: time.Millisecond * 50,
	}
	wn.setup()
	wn.eventualReadyDelay = time.Second

	netA := wn
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	noAddressConfig := defaultConfig
	noAddressConfig.NetAddress = ""
	netB := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)

	var peers []*wsPeer
	peers = netA.peerSnapshot(peers)
	require.Equalf(t, len(peers), 1, "Expected number of peers should be 1")
	peer := peers[0]
	// modify the peer on netA and
	atomic.StoreInt64(&peer.intermittentOutgoingMessageEnqueueTime, time.Now().Add(-maxMessageQueueDuration).Add(-time.Second).UnixNano())
	// wait up to 2*slowWritingPeerMonitorInterval for the monitor to figure out it needs to disconnect.
	expire := time.Now().Add(maxMessageQueueDuration * time.Duration(2))
	for {
		peers = netA.peerSnapshot(peers)
		if len(peers) == 0 || peers[0] != peer {
			break
		}
		if time.Now().After(expire) {
			require.Fail(t, "Slow peer was not disconnected")
		}
		time.Sleep(time.Millisecond * 5)
	}
}

func TestForceMessageRelaying(t *testing.T) {
	log := logging.TestingLog(t)
	log.SetLevel(logging.Level(defaultConfig.BaseLoggerDebugLevel))
	wn := &WebsocketNetwork{
		log:       log,
		config:    defaultConfig,
		phonebook: emptyPhonebookSingleton,
		GenesisID: "go-test-network-genesis",
		NetworkID: config.Devtestnet,
	}
	wn.setup()
	wn.eventualReadyDelay = time.Second

	netA := wn
	netA.config.GossipFanout = 1

	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	counter := newMessageCounter(t, 5)
	counterDone := counter.done
	netA.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: counter}})
	netA.Start()
	addrA, postListen := netA.Address()
	require.Truef(t, postListen, "Listening network failed to start")

	noAddressConfig := defaultConfig
	noAddressConfig.NetAddress = ""
	netB := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netB.config.GossipFanout = 1
	netB.phonebook = &oneEntryPhonebook{addrA}
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	noAddressConfig.ForceRelayMessages = true
	netC := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netC.config.GossipFanout = 1
	netC.phonebook = &oneEntryPhonebook{addrA}
	netC.Start()
	defer func() { t.Log("stopping C"); netB.Stop(); t.Log("C done") }()

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)
	waitReady(t, netC, readyTimeout.C)

	// send 5 messages from both netB and netC to netA
	for i := 0; i < 5; i++ {
		err := netB.Relay(context.Background(), debugTag, []byte{1, 2, 3}, true, nil)
		require.NoError(t, err)
		err = netC.Relay(context.Background(), debugTag, []byte{1, 2, 3}, true, nil)
		require.NoError(t, err)
	}

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		if counter.count < 5 {
			require.Failf(t, "One or more messages failed to reach destination network", "%d > %d", 5, counter.count)
		} else if counter.count > 5 {
			require.Failf(t, "One or more messages that were expected to be dropped, reached destination network", "%d < %d", 5, counter.count)
		}
	}
	netA.ClearHandlers()
	counter = newMessageCounter(t, 10)
	counterDone = counter.done
	netA.RegisterHandlers([]TaggedMessageHandler{TaggedMessageHandler{Tag: debugTag, MessageHandler: counter}})

	// hack the relayMessages on the netB so that it would start sending messages.
	netB.relayMessages = true
	// send additional 10 messages from netB
	for i := 0; i < 10; i++ {
		err := netB.Relay(context.Background(), debugTag, []byte{1, 2, 3}, true, nil)
		require.NoError(t, err)
	}

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		require.Failf(t, "One or more messages failed to reach destination network", "%d > %d", 10, counter.count)
	}

}
