package packet

import (
	"runtime"
	"sync"

	"pm.cn.tuwien.ac.at/ipfix/go-flows/flows"
)

type EventTable interface {
	Event(buffer *shallowMultiPacketBuffer)
	Expire()
	EOF(flows.DateTimeNanoSeconds)
	KeyFunc() func(PacketBuffer) (flows.FlowKey, bool)
}

type baseTable struct {
	key func(PacketBuffer) (flows.FlowKey, bool)
}

func (bt *baseTable) KeyFunc() func(PacketBuffer) (flows.FlowKey, bool) {
	return bt.key
}

type ParallelFlowTable struct {
	baseTable
	tables     []*flows.FlowTable
	expire     []chan struct{}
	expirewg   sync.WaitGroup
	buffers    []*shallowMultiPacketBufferRing
	tmp        []*shallowMultiPacketBuffer
	wg         sync.WaitGroup
	expireTime flows.DateTimeNanoSeconds
	nextExpire flows.DateTimeNanoSeconds
}

type SingleFlowTable struct {
	baseTable
	table      *flows.FlowTable
	buffer     *shallowMultiPacketBufferRing
	expire     chan struct{}
	done       chan struct{}
	expireTime flows.DateTimeNanoSeconds
	nextExpire flows.DateTimeNanoSeconds
}

func (sft *SingleFlowTable) Expire() {
	sft.expire <- struct{}{}
	go runtime.GC()
}

func (sft *SingleFlowTable) Event(buffer *shallowMultiPacketBuffer) {
	current := buffer.Timestamp()
	if current > sft.nextExpire {
		sft.Expire()
		sft.nextExpire = current + sft.expireTime
	}
	b, _ := sft.buffer.popEmpty()
	buffer.Copy(b)
	b.finalize()
}

func (sft *SingleFlowTable) EOF(now flows.DateTimeNanoSeconds) {
	close(sft.buffer.full)
	<-sft.done
	sft.table.EOF(now)
}

func NewParallelFlowTable(num int, features flows.RecordListMaker, newflow flows.FlowCreator, options flows.FlowOptions, expire flows.DateTimeNanoSeconds, selector DynamicKeySelector) EventTable {
	bt := baseTable{}
	switch {
	case selector.fivetuple:
		bt.key = fivetuple
	case selector.empty:
		bt.key = makeEmptyKey
	default:
		bt.key = selector.makeDynamicKey
	}
	if num == 1 {
		ret := &SingleFlowTable{
			baseTable:  bt,
			table:      flows.NewFlowTable(features, newflow, options, selector.fivetuple),
			expireTime: expire,
		}
		ret.buffer = newShallowMultiPacketBufferRing(fullBuffers, batchSize)
		ret.expire = make(chan struct{}, 1)
		ret.done = make(chan struct{})
		go func() {
			t := ret.table
			defer close(ret.done)
			for {
				select {
				case <-ret.expire:
					t.Expire()
				case buffer, ok := <-ret.buffer.full:
					if !ok {
						return
					}
					for {
						b := buffer.read()
						if b == nil {
							break
						}
						t.Event(b)
					}
					buffer.recycle()
				}
			}
		}()
		return ret
	}
	ret := &ParallelFlowTable{
		baseTable:  bt,
		tables:     make([]*flows.FlowTable, num),
		buffers:    make([]*shallowMultiPacketBufferRing, num),
		tmp:        make([]*shallowMultiPacketBuffer, num),
		expire:     make([]chan struct{}, num),
		expireTime: expire,
	}
	for i := 0; i < num; i++ {
		c := newShallowMultiPacketBufferRing(fullBuffers, batchSize)
		expire := make(chan struct{}, 1)
		ret.expire[i] = expire
		ret.buffers[i] = c
		t := flows.NewFlowTable(features, newflow, options, selector.fivetuple)
		ret.tables[i] = t
		ret.wg.Add(1)
		go func() {
			defer ret.wg.Done()
			for {
				select {
				case <-expire:
					t.Expire()
					ret.expirewg.Done()
				case buffer, ok := <-c.full:
					if !ok {
						return
					}
					for {
						b := buffer.read()
						if b == nil {
							break
						}
						t.Event(b)
					}
					buffer.recycle()
				}
			}
		}()
	}
	return ret
}

func (pft *ParallelFlowTable) Expire() {
	for _, e := range pft.expire {
		pft.expirewg.Add(1)
		e <- struct{}{}
	}
	pft.expirewg.Wait()
	go runtime.GC()
}

func (pft *ParallelFlowTable) Event(buffer *shallowMultiPacketBuffer) {
	current := buffer.Timestamp()
	if current > pft.nextExpire {
		pft.Expire()
		pft.nextExpire = current + pft.expireTime
	}
	num := len(pft.tables)

	for i := 0; i < num; i++ {
		pft.tmp[i], _ = pft.buffers[i].popEmpty()
	}
	for {
		b := buffer.read()
		if b == nil {
			break
		}
		h := b.Key().Hash() % uint64(num)
		pft.tmp[h].push(b)
	}
	for i := 0; i < num; i++ {
		pft.tmp[i].finalize()
	}
}

func (pft *ParallelFlowTable) EOF(now flows.DateTimeNanoSeconds) {
	for _, c := range pft.buffers {
		close(c.full)
	}
	pft.wg.Wait()
	for _, t := range pft.tables {
		pft.wg.Add(1)
		go func(table *flows.FlowTable) {
			defer pft.wg.Done()
			table.EOF(now)
		}(t)
	}
	pft.wg.Wait()
}
