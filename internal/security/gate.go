// Package security contains bounded admission controls shared by both services.
package security

import "sync"

type Gate struct {
	once  sync.Once
	slots chan struct{}
}

func (g *Gate) Try() bool {
	g.once.Do(func() { g.slots = make(chan struct{}, 128) })
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gate) Leave() { <-g.slots }
