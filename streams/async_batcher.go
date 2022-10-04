// Copyright 2022 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package streams

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/aws/go-kafka-event-source/streams/sak"
)

type BatchItem[K comparable, V any] struct {
	batch unsafe.Pointer
	Key   K
	Value V
}

func batchFor[S any, K comparable, V any](bi BatchItem[K, V]) *Batch[S, K, V] {
	return (*Batch[S, K, V])(bi.batch)
}

type Batch[S any, K comparable, V any] struct {
	EventContext *EventContext[S]
	Items        []BatchItem[K, V]
	callback     func(*Batch[S, K, V])
	completed    int64
}

func NewBatch[S any, K comparable, V any](ec *EventContext[S], cb func(*Batch[S, K, V])) *Batch[S, K, V] {
	return &Batch[S, K, V]{
		EventContext: ec,
		callback:     cb,
	}
}

func (b *Batch[S, K, V]) completeItem() {
	if atomic.AddInt64(&b.completed, 1) == int64(len(b.Items)) {
		if b.callback != nil {
			b.callback(b)
		} else {
			b.EventContext.AsyncJobComplete(func() (ExecutionState, error) {
				return Complete, nil
			})
		}
	}
}

func (b *Batch[S, K, V]) Add(items ...BatchItem[K, V]) *Batch[S, K, V] {
	for i := range items {
		items[i].batch = unsafe.Pointer(b)
	}
	b.Items = append(b.Items, items...)
	return b
}

func (b *Batch[S, K, V]) AddKeyValue(key K, value V) *Batch[S, K, V] {
	b.Items = append(b.Items, BatchItem[K, V]{batch: unsafe.Pointer(b), Key: key, Value: value})
	return b
}

type BatchExecutor[K comparable, V any] func(batch []BatchItem[K, V])

type asyncBatchState int

const (
	batcherReady asyncBatchState = iota
	batcherExecuting
)

type asyncBatch[K comparable, V any] struct {
	items      []BatchItem[K, V]
	state      asyncBatchState
	flushTimer *time.Timer
}

func (b *asyncBatch[K, V]) add(item BatchItem[K, V]) {
	b.items = append(b.items, item)
}

func (b *asyncBatch[K, V]) reset(assignments map[K]*asyncBatch[K, V]) {
	var empty BatchItem[K, V]
	for i, item := range b.items {
		delete(assignments, item.Key)
		b.items[i] = empty
	}
	b.items = b.items[0:0]
	b.state = batcherReady
}

type AsyncBatcher[S any, K comparable, V any] struct {
	batches        []*asyncBatch[K, V]
	assignments    map[K]*asyncBatch[K, V]
	pendingItems   *sak.List[BatchItem[K, V]]
	executor       BatchExecutor[K, V]
	executingCount int
	MaxBatchSize   int
	BatchDelay     time.Duration
	mux            sync.Mutex
}

func NewAsyncBatcher[S StateStore, K comparable, V any](eventSource *EventSource[S], executor BatchExecutor[K, V], maxBatchSize, maxConcurrentBatches int, delay time.Duration) *AsyncBatcher[S, K, V] {
	batches := make([]*asyncBatch[K, V], maxConcurrentBatches)
	for i := range batches {
		batches[i] = &asyncBatch[K, V]{
			items: make([]BatchItem[K, V], 0, maxBatchSize),
		}
	}

	if delay == 0 {
		delay = time.Millisecond * 5
	}
	return &AsyncBatcher[S, K, V]{
		executor:     executor,
		assignments:  make(map[K]*asyncBatch[K, V]),
		pendingItems: sak.NewList[BatchItem[K, V]](),
		batches:      batches,
		MaxBatchSize: maxBatchSize,
		BatchDelay:   sak.Abs(delay),
	}
}

func (ab *AsyncBatcher[S, K, V]) Add(batch *Batch[S, K, V]) {
	for _, item := range batch.Items {
		ab.add(item)
	}
}

func (ab *AsyncBatcher[S, K, V]) add(bi BatchItem[K, V]) {
	ab.mux.Lock()
	if batch := ab.batchFor(bi); batch != nil {
		ab.addToBatch(bi, batch)
	} else {
		ab.pendingItems.PushBack(bi)
	}
	ab.mux.Unlock()
}

func (ab *AsyncBatcher[S, K, V]) batchFor(item BatchItem[K, V]) *asyncBatch[K, V] {
	if batch, ok := ab.assignments[item.Key]; ok && batch.state == batcherReady {
		return batch
	} else if ok {
		// this key is currently in an executing batch, so we have to wait for it to finish
		return nil
	}
	for _, batch := range ab.batches {
		if batch.state == batcherReady {
			return batch
		}
	}
	return nil
}

func (ab *AsyncBatcher[S, K, V]) addToBatch(item BatchItem[K, V], batch *asyncBatch[K, V]) {
	ab.assignments[item.Key] = batch
	batch.add(item)

	if len(batch.items) == ab.MaxBatchSize {
		ab.conditionallyExecuteBatch(batch)
	} else if batch.flushTimer == nil {
		batch.flushTimer = time.AfterFunc(ab.BatchDelay, func() {
			// we have a race condition where we could have reached max items
			ab.mux.Lock()
			ab.conditionallyExecuteBatch(batch)
			ab.mux.Unlock()
		})
	}
}

func (ab *AsyncBatcher[S, K, V]) conditionallyExecuteBatch(batch *asyncBatch[K, V]) {
	if batch.state == batcherReady {
		batch.state = batcherExecuting
		ab.executingCount++
		if batch.flushTimer != nil {
			batch.flushTimer.Stop()
			batch.flushTimer = nil
		}
		go ab.executeBatch(batch)
	}
}

func (ab *AsyncBatcher[S, K, V]) executeBatch(batch *asyncBatch[K, V]) {
	ab.executor(batch.items)
	for _, item := range batch.items {
		batchFor[S](item).completeItem()
	}
	ab.mux.Lock()
	ab.executingCount--
	// TODO: handle errors right here as this may effect other batches
	batch.reset(ab.assignments)
	ab.flushPendingItems()
	ab.mux.Unlock()
}

func (ab *AsyncBatcher[S, K, V]) flushPendingItems() {
	for el := ab.pendingItems.Front(); el != nil; {
		if batch := ab.batchFor(el.Value); batch != nil {
			ab.addToBatch(el.Value, batch)
			if ab.executingCount == len(ab.batches) {
				// there are no available batches, no need to continue in this loop
				return
			}
			tmp := el.Next()
			ab.pendingItems.Remove(el)
			el = tmp
		} else {
			el = el.Next()
		}
	}
}
