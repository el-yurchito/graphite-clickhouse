package rollup

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	"github.com/lomik/zapwriter"
	"go.uber.org/zap"
)

type Rollup struct {
	mu               sync.RWMutex
	rules            *Rules
	addr             string
	table            string
	defaultPrecision uint32
	interval         time.Duration
}

func ReadFromXMLFile(filename string, defaultPrecision uint32) (*Rollup, error) {
	rollupConfBody, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	rules, err := ParseXML(rollupConfBody)
	if err != nil {
		return nil, err
	}

	if defaultPrecision > 0 {
		rules.addDefaultPrecision(defaultPrecision)
	}

	return &Rollup{rules: rules}, nil
}

func Auto(addr string, table string, interval time.Duration, defaultPrecision uint32) (*Rollup, error) {
	r := &Rollup{
		addr:             addr,
		table:            table,
		interval:         interval,
		defaultPrecision: defaultPrecision,
	}

	err := r.update()
	if err != nil {
		return nil, err
	}

	go r.updateWorker()

	return r, nil
}

func (r *Rollup) Rules() *Rules {
	r.mu.RLock()
	rules := r.rules
	r.mu.RUnlock()
	return rules
}

func (r *Rollup) update() error {
	rules, err := remoteLoad(r.addr, r.table)
	if err != nil {
		zapwriter.Logger("rollup").Error(fmt.Sprintf("rollup rules update failed for table %#v", r.table), zap.Error(err))
		return err
	}

	if r.defaultPrecision > 0 {
		rules.addDefaultPrecision(r.defaultPrecision)
	}

	r.mu.Lock()
	r.rules = rules
	r.mu.Unlock()
	return nil
}

func (r *Rollup) updateWorker() {
	for {
		time.Sleep(r.interval)
		r.update()
	}
}

func (r *Rollup) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.Rules())
}
