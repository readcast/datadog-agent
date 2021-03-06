// +build linux
// +build !android

package netlink

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DataDog/datadog-agent/pkg/network"
	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	ct "github.com/florianl/go-conntrack"
	"golang.org/x/sys/unix"
)

const (
	initializationTimeout = time.Second * 10

	compactInterval = time.Minute
)

// Conntracker is a wrapper around go-conntracker that keeps a record of all connections in user space
type Conntracker interface {
	GetTranslationForConn(network.ConnectionStats) *network.IPTranslation
	DeleteTranslation(network.ConnectionStats)
	GetStats() map[string]int64
	Close()
}

type connKey struct {
	srcIP   util.Address
	srcPort uint16

	dstIP   util.Address
	dstPort uint16

	// the transport protocol of the connection, using the same values as specified in the agent payload.
	transport network.ConnectionType
}

type realConntracker struct {
	sync.RWMutex
	consumer *Consumer
	state    map[connKey]*network.IPTranslation

	// The maximum size the state map will grow before we reject new entries
	maxStateSize int

	compactTicker *time.Ticker
	stats         struct {
		gets                 int64
		getTimeTotal         int64
		registers            int64
		registersDropped     int64
		registersTotalTime   int64
		unregisters          int64
		unregistersTotalTime int64
	}
	exceededSizeLogLimit *util.LogLimit
}

// NewConntracker creates a new conntracker with a short term buffer capped at the given size
func NewConntracker(procRoot string, maxStateSize, targetRateLimit int, listenAllNamespaces bool) (Conntracker, error) {
	var (
		err         error
		conntracker Conntracker
	)

	done := make(chan struct{})

	go func() {
		conntracker, err = newConntrackerOnce(procRoot, maxStateSize, targetRateLimit, listenAllNamespaces)
		done <- struct{}{}
	}()

	select {
	case <-done:
		return conntracker, err
	case <-time.After(initializationTimeout):
		return nil, fmt.Errorf("could not initialize conntrack after: %s", initializationTimeout)
	}
}

func newConntrackerOnce(procRoot string, maxStateSize, targetRateLimit int, listenAllNamespaces bool) (Conntracker, error) {
	consumer, err := NewConsumer(procRoot, targetRateLimit, listenAllNamespaces)
	if err != nil {
		return nil, err
	}

	ctr := &realConntracker{
		consumer:             consumer,
		compactTicker:        time.NewTicker(compactInterval),
		state:                make(map[connKey]*network.IPTranslation),
		maxStateSize:         maxStateSize,
		exceededSizeLogLimit: util.NewLogLimit(10, time.Minute*10),
	}

	ctr.loadInitialState(consumer.DumpTable(unix.AF_INET))
	ctr.loadInitialState(consumer.DumpTable(unix.AF_INET6))
	ctr.run()
	log.Infof("initialized conntrack with target_rate_limit=%d messages/sec", targetRateLimit)
	return ctr, nil
}

func (ctr *realConntracker) GetTranslationForConn(c network.ConnectionStats) *network.IPTranslation {
	then := time.Now().UnixNano()

	ctr.RLock()
	defer ctr.RUnlock()

	k := connKey{
		srcIP:     c.Source,
		srcPort:   c.SPort,
		dstIP:     c.Dest,
		dstPort:   c.DPort,
		transport: c.Type,
	}

	result := ctr.state[k]

	now := time.Now().UnixNano()
	atomic.AddInt64(&ctr.stats.gets, 1)
	atomic.AddInt64(&ctr.stats.getTimeTotal, now-then)
	return result
}

func (ctr *realConntracker) GetStats() map[string]int64 {
	// only a few stats are locked
	ctr.RLock()
	size := len(ctr.state)
	ctr.RUnlock()

	m := map[string]int64{
		"state_size": int64(size),
	}

	if ctr.stats.gets != 0 {
		m["gets_total"] = ctr.stats.gets
		m["nanoseconds_per_get"] = ctr.stats.getTimeTotal / ctr.stats.gets
	}
	if ctr.stats.registers != 0 {
		m["registers_total"] = ctr.stats.registers
		m["registers_dropped"] = ctr.stats.registersDropped
		m["nanoseconds_per_register"] = ctr.stats.registersTotalTime / ctr.stats.registers
	}
	if ctr.stats.unregisters != 0 {
		m["unregisters_total"] = ctr.stats.unregisters
		m["nanoseconds_per_unregister"] = ctr.stats.unregistersTotalTime / ctr.stats.unregisters
	}

	// Merge telemetry from the consumer
	for k, v := range ctr.consumer.GetStats() {
		m[k] = v
	}

	return m
}

func (ctr *realConntracker) DeleteTranslation(c network.ConnectionStats) {
	then := time.Now().UnixNano()
	defer func() {
		atomic.AddInt64(&ctr.stats.unregistersTotalTime, time.Now().UnixNano()-then)
	}()

	ctr.Lock()
	defer ctr.Unlock()

	keys := []connKey{
		{
			srcIP:     c.Source,
			srcPort:   c.SPort,
			dstIP:     c.Dest,
			dstPort:   c.DPort,
			transport: c.Type,
		},
		{
			srcIP:     c.Dest,
			srcPort:   c.DPort,
			dstIP:     c.Source,
			dstPort:   c.SPort,
			transport: c.Type,
		},
	}

	deleteTrans := func(k connKey) bool {
		t, ok := ctr.state[k]
		if !ok {
			log.Tracef("not deleting %+v from conntrack", k)
			return false
		}

		delete(ctr.state, k)
		delete(ctr.state, ipTranslationToConnKey(k.transport, t))
		log.Tracef("deleted %+v from conntrack", k)
		return true
	}

	for _, k := range keys {
		if ok := deleteTrans(k); ok {
			atomic.AddInt64(&ctr.stats.unregisters, 1)
			break
		}
	}
}

func (ctr *realConntracker) Close() {
	ctr.consumer.Stop()
	ctr.compactTicker.Stop()
	ctr.exceededSizeLogLimit.Close()
}

func (ctr *realConntracker) loadInitialState(events <-chan Event) {
	for e := range events {
		conns := DecodeAndReleaseEvent(e)
		for _, c := range conns {
			if len(ctr.state) < ctr.maxStateSize && isNAT(c) {
				log.Tracef("%s", c)
				if k, ok := formatKey(c.Origin); ok {
					ctr.state[k] = formatIPTranslation(c.Reply)
				}
				if k, ok := formatKey(c.Reply); ok {
					ctr.state[k] = formatIPTranslation(c.Origin)
				}
			}
		}
	}
}

// register is registered to be called whenever a conntrack update/create is called.
// it will keep being called until it returns nonzero.
func (ctr *realConntracker) register(c Con) int {
	// don't bother storing if the connection is not NAT
	if !isNAT(c) {
		atomic.AddInt64(&ctr.stats.registersDropped, 1)
		return 0
	}

	now := time.Now().UnixNano()
	registerTuple := func(keyTuple, transTuple *ct.IPTuple) {
		key, ok := formatKey(keyTuple)
		if !ok {
			return
		}

		if len(ctr.state) >= ctr.maxStateSize {
			ctr.logExceededSize()
			return
		}

		ctr.state[key] = formatIPTranslation(transTuple)
	}

	log.Tracef("%s", c)

	ctr.Lock()
	defer ctr.Unlock()
	registerTuple(c.Origin, c.Reply)
	registerTuple(c.Reply, c.Origin)
	then := time.Now()
	atomic.AddInt64(&ctr.stats.registers, 1)
	atomic.AddInt64(&ctr.stats.registersTotalTime, then.UnixNano()-now)

	return 0
}

func (ctr *realConntracker) logExceededSize() {
	if ctr.exceededSizeLogLimit.ShouldLog() {
		log.Warnf("exceeded maximum conntrack state size: %d entries. You may need to increase system_probe_config.conntrack_max_state_size (will log first ten times, and then once every 10 minutes)", ctr.maxStateSize)
	}
}

func (ctr *realConntracker) run() {
	go func() {
		events := ctr.consumer.Events()
		for e := range events {
			conns := DecodeAndReleaseEvent(e)
			for _, c := range conns {
				ctr.register(c)
			}
		}
	}()

	go func() {
		for range ctr.compactTicker.C {
			ctr.compact()
		}
	}()
}

func (ctr *realConntracker) compact() {
	ctr.Lock()
	defer ctr.Unlock()

	// https://github.com/golang/go/issues/20135
	copied := make(map[connKey]*network.IPTranslation, len(ctr.state))
	for k, v := range ctr.state {
		copied[k] = v
	}
	ctr.state = copied
}

func isNAT(c Con) bool {
	if c.Origin == nil ||
		c.Reply == nil ||
		c.Origin.Proto == nil ||
		c.Reply.Proto == nil ||
		c.Origin.Proto.SrcPort == nil ||
		c.Origin.Proto.DstPort == nil ||
		c.Reply.Proto.SrcPort == nil ||
		c.Reply.Proto.DstPort == nil {
		return false
	}

	return !(*c.Origin.Src).Equal(*c.Reply.Dst) ||
		!(*c.Origin.Dst).Equal(*c.Reply.Src) ||
		*c.Origin.Proto.SrcPort != *c.Reply.Proto.DstPort ||
		*c.Origin.Proto.DstPort != *c.Reply.Proto.SrcPort
}

func formatIPTranslation(tuple *ct.IPTuple) *network.IPTranslation {
	srcIP := *tuple.Src
	dstIP := *tuple.Dst

	srcPort := *tuple.Proto.SrcPort
	dstPort := *tuple.Proto.DstPort

	return &network.IPTranslation{
		ReplSrcIP:   util.AddressFromNetIP(srcIP),
		ReplDstIP:   util.AddressFromNetIP(dstIP),
		ReplSrcPort: srcPort,
		ReplDstPort: dstPort,
	}
}

func formatKey(tuple *ct.IPTuple) (k connKey, ok bool) {
	ok = true
	k.srcIP = util.AddressFromNetIP(*tuple.Src)
	k.dstIP = util.AddressFromNetIP(*tuple.Dst)
	k.srcPort = *tuple.Proto.SrcPort
	k.dstPort = *tuple.Proto.DstPort

	proto := *tuple.Proto.Number
	switch proto {
	case unix.IPPROTO_TCP:
		k.transport = network.TCP
	case unix.IPPROTO_UDP:
		k.transport = network.UDP
	default:
		ok = false
	}

	return
}

func ipTranslationToConnKey(proto network.ConnectionType, t *network.IPTranslation) connKey {
	return connKey{
		srcIP:     t.ReplSrcIP,
		dstIP:     t.ReplDstIP,
		srcPort:   t.ReplSrcPort,
		dstPort:   t.ReplDstPort,
		transport: proto,
	}
}
