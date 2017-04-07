package flows

import "sync"

type FlowCreator func(Event, *FlowTable, FlowKey, Time) Flow
type FeatureListCreator func() *FeatureList

type FlowTable struct {
	flows         map[FlowKey]Flow
	newflow       FlowCreator
	activeTimeout Time
	idleTimeout   Time
	now           Time
	timerPool     sync.Pool
	featurePool   sync.Pool
	DataStore     interface{}
	eof           bool
}

func NewFlowTable(features FeatureListCreator, newflow FlowCreator, activeTimeout, idleTimeout Time) *FlowTable {
	return &FlowTable{
		flows:         make(map[FlowKey]Flow, 1000000),
		newflow:       newflow,
		activeTimeout: activeTimeout,
		idleTimeout:   idleTimeout,
		timerPool: sync.Pool{
			New: func() interface{} {
				return new(funcEntry)
			},
		},
		featurePool: sync.Pool{
			New: func() interface{} {
				return features()
			},
		},
	}
}

func (tab *FlowTable) Expire() {
	when := tab.now
	for _, elem := range tab.flows {
		if when > elem.NextEvent() {
			elem.Expire(when)
		}
	}
}

func (tab *FlowTable) Event(event Event) {
	when := event.Timestamp()
	key := event.Key()

	tab.now = when

	elem, ok := tab.flows[key]
	if ok {
		if when > elem.NextEvent() {
			elem.Expire(when)
			ok = elem.Active()
		}
	}
	if !ok {
		elem = tab.newflow(event, tab, key, when)
		tab.flows[key] = elem
	}
	elem.Event(event, when)
}

func (tab *FlowTable) Remove(entry Flow) {
	if !tab.eof {
		delete(tab.flows, entry.Key())
		entry.Recycle()
	}
}

func (tab *FlowTable) EOF(now Time) {
	tab.eof = true
	for _, v := range tab.flows {
		if now > v.NextEvent() {
			v.Expire(now)
		}
		if v.Active() {
			v.EOF(now)
		}
	}
	tab.flows = make(map[FlowKey]Flow)
	tab.eof = false
}
