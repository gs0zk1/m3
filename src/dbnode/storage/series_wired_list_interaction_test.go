// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package storage

import (
	"sync"
	"testing"
	"time"

	"github.com/m3db/m3/src/dbnode/clock"
	"github.com/m3db/m3/src/dbnode/runtime"
	"github.com/m3db/m3/src/dbnode/storage/block"
	"github.com/m3db/m3/src/dbnode/namespace"
	"github.com/m3db/m3/src/dbnode/storage/series"
	"github.com/m3db/m3/src/dbnode/storage/series/lookup"
	"github.com/m3db/m3/src/dbnode/ts"
	"github.com/m3db/m3/src/x/context"
	"github.com/m3db/m3/src/x/ident"
	"github.com/m3db/m3/src/x/instrument"
	"github.com/m3db/m3/src/x/pool"

	"github.com/stretchr/testify/require"
)

// TestSeriesWiredListConcurrentInteractions was added as a regression test
// after discovering that interactions between a single series and the wired
// list could trigger a mutual dead lock. Specifically, if the wired list event
// channel was full, then the series could get blocked on a call to list.Update()
// in the OnRetrieveBlockMethod while the only goroutine pulling items off of that
// channel was stuck on the same series OnEvictedFromWiredList method. In that case,
// the OnRetrieveBlockMethod was stuck on a channel send while holding a lock that was
// required for the OnEvictedFromWiredList method that the wired list worker routine
// was calling.
func TestSeriesWiredListConcurrentInteractions(t *testing.T) {
	var (
		runtimeOptsMgr = runtime.NewOptionsManager()
		runtimeOpts    = runtime.NewOptions().SetMaxWiredBlocks(1)
	)
	runtimeOptsMgr.Update(runtimeOpts)

	runtime.NewOptions().SetMaxWiredBlocks(1)
	wl := block.NewWiredList(block.WiredListOptions{
		RuntimeOptionsManager: runtimeOptsMgr,
		InstrumentOptions:     instrument.NewOptions(),
		ClockOptions:          clock.NewOptions(),
		// Use a small channel to stress-test the implementation
		EventsChannelSize: 1,
	})
	wl.Start()
	defer wl.Stop()

	var (
		blOpts = testDatabaseOptions().DatabaseBlockOptions()
		blPool = block.NewDatabaseBlockPool(
			// Small pool size to make any pooling issues more
			// likely to manifest.
			pool.NewObjectPoolOptions().SetSize(5),
		)
	)
	blPool.Init(func() block.DatabaseBlock {
		return block.NewDatabaseBlock(time.Time{}, 0, ts.Segment{}, blOpts, namespace.Context{})
	})

	var (
		opts = testDatabaseOptions().SetDatabaseBlockOptions(
			blOpts.
				SetWiredList(wl).
				SetDatabaseBlockPool(blPool),
		)
		shard  = testDatabaseShard(t, opts)
		id     = ident.StringID("foo")
		series = series.NewDatabaseSeries(id, ident.Tags{}, shard.seriesOpts)
	)

	series.Reset(id, ident.Tags{}, nil, shard.seriesOnRetrieveBlock, shard, shard.seriesOpts)
	series.Bootstrap(nil)
	shard.Lock()
	shard.insertNewShardEntryWithLock(lookup.NewEntry(series, 0))
	shard.Unlock()

	var (
		wg        = sync.WaitGroup{}
		doneCh    = make(chan struct{})
		blockSize = 2 * time.Hour
	)
	go func() {
		// Try and trigger any pooling issues
		for {
			select {
			case <-doneCh:
				return
			default:
				bl := blPool.Get()
				bl.Close()
			}
		}
	}()

	var (
		start          = time.Now().Truncate(blockSize)
		startLock      = sync.Mutex{}
		getAndIncStart = func() time.Time {
			startLock.Lock()
			t := start
			start = start.Add(blockSize)
			startLock.Unlock()
			return t
		}
	)

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			blTime := getAndIncStart()
			shard.OnRetrieveBlock(id, nil, blTime, ts.Segment{}, namespace.Context{})
			// Simulate concurrent reads
			_, err := shard.ReadEncoded(context.NewContext(), id, blTime, blTime.Add(blockSize), namespace.Context{})
			require.NoError(t, err)
			wg.Done()
		}()
	}

	wg.Wait()
	close(doneCh)
}
