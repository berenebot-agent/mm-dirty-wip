package events

import (
	"sync"

	"gatehouse-mail/internal/model"
)

type Hub struct {
	mu sync.RWMutex
	next int
	subs map[int]chan model.Event
}
func NewHub()*Hub{return &Hub{subs:map[int]chan model.Event{}}}
func (h *Hub) Subscribe(buffer int)(int,<-chan model.Event,func()){if buffer<1{buffer=16};h.mu.Lock();id:=h.next;h.next++;ch:=make(chan model.Event,buffer);h.subs[id]=ch;h.mu.Unlock();cancel:=func(){h.mu.Lock();if c,ok:=h.subs[id];ok{delete(h.subs,id);close(c)};h.mu.Unlock()};return id,ch,cancel}
func (h *Hub) Publish(e model.Event){h.mu.RLock();defer h.mu.RUnlock();for _,ch:=range h.subs{select{case ch<-e:default:}}}
