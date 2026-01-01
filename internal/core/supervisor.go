package core

import (
	"context"
	"sync"
)

type Supervisor struct {
	wg     sync.WaitGroup
	cancel context.CancelFunc
	ctx    context.Context
}

func NewSupervisor(parent context.Context) *Supervisor {
	ctx, cancel := context.WithCancel(parent)
	return &Supervisor{ctx: ctx, cancel: cancel}
}

func (s *Supervisor) Context() context.Context { return s.ctx }

func (s *Supervisor) Go(fn func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(s.ctx)
	}()
}

func (s *Supervisor) Stop() {
	s.cancel()
	s.wg.Wait()
}
