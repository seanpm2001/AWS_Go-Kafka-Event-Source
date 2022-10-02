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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/go-kafka-event-source/streams/sak"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Returned by an EventProcessor or Interjector in response to an EventContext. ExecutionState
// should not be conflated with concepts of error state, such as Success or Failure.
type ExecutionState int

const (
	// Complete signals the EventSource that the event or interjection is completely processed.
	// Once Complete is returned, the offset for the associated EventContext will be commited.
	Complete ExecutionState = 0
	// Incomplete signals the EventSource that the event or interjection is still ongoing, and
	// that your application promises to fulfill the EventContext in the future.
	// The offset for the associated EventContext will not be commited.
	Incomplete ExecutionState = 1

	Fatal       ExecutionState = 2
	unknownType ExecutionState = 3
)

type partitionWorker[T StateStore] struct {
	eosProducer         *eosProducerPool[T]
	partitionInput      chan []*kgo.Record
	eventInput          chan *EventContext[T]
	asyncCompleter      asyncCompleter[T]
	interjectionChannel chan *interjection[T]
	stopSignal          chan struct{}
	revokedSignal       chan struct{}
	stopped             chan struct{}
	changeLog           changeLogPartition[T]
	eventSource         *EventSource[T]
	runStatus           sak.RunStatus
	pending             int64
	processed           int64
	highestOffset       int64
	topicPartition      TopicPartition
	revocationWaiter    sync.WaitGroup
}

func newPartitionWorker[T StateStore](
	eventSource *EventSource[T],
	topicPartition TopicPartition,
	commitLog *eosCommitLog,
	changeLog changeLogPartition[T],
	eosProducer *eosProducerPool[T],
	waiter func()) *partitionWorker[T] {

	eosConfig := eventSource.source.config.EosConfig

	recordsInputSize := sak.Max(eosConfig.MaxBatchSize/10, 10000)
	asyncSize := recordsInputSize * 4
	pw := &partitionWorker[T]{
		eventSource:    eventSource,
		topicPartition: topicPartition,
		changeLog:      changeLog,
		eosProducer:    eosProducer,
		stopSignal:     make(chan struct{}),
		revokedSignal:  make(chan struct{}),
		stopped:        make(chan struct{}),
		asyncCompleter: asyncCompleter[T]{
			asyncJobs:      make(chan asyncJob[T], asyncSize),
			asyncFullReply: make(chan struct{}, 1),
		},
		partitionInput:      make(chan []*kgo.Record, 128),
		eventInput:          make(chan *EventContext[T], recordsInputSize),
		interjectionChannel: make(chan *interjection[T], 1),
		runStatus:           eventSource.runStatus.Fork(),
		highestOffset:       -1,
	}

	go pw.work(pw.eventSource.interjections, waiter, commitLog)

	return pw
}

func (pw *partitionWorker[T]) add(records []*kgo.Record) {
	if pw.isRevoked() {
		return
	}
	atomic.AddInt64(&pw.pending, int64(len(records)))
	pw.partitionInput <- records
}

func (pw *partitionWorker[T]) revoke() {
	pw.runStatus.Halt()
}

type sincer struct {
	then time.Time
}

func (s sincer) String() string {
	return fmt.Sprintf("%v", time.Since(s.then))
}

func (pw *partitionWorker[T]) pushRecords() {
	for {
		select {
		case records := <-pw.partitionInput:
			if !pw.isRevoked() {
				pw.scheduleTxnAndExecution(records)
			}
		case <-pw.runStatus.Done():
			log.Debugf("Closing worker for %+v", pw.topicPartition)
			pw.stopSignal <- struct{}{}
			<-pw.stopped
			close(pw.partitionInput)
			close(pw.eventInput)
			close(pw.asyncCompleter.asyncJobs)
			log.Debugf("Closed worker for %+v", pw.topicPartition)
			return
		}
	}
}

func (pw *partitionWorker[T]) scheduleTxnAndExecution(records []*kgo.Record) {
	pw.revocationWaiter.Add(len(records)) // optimistically do one add call
	for _, record := range records {
		if record != nil && record.Offset >= pw.highestOffset {
			ec := newEventContext(pw.runStatus.Ctx(), record, pw.changeLog.changeLogData(), pw)
			pw.eosProducer.addEventContext(ec)
			pw.eventInput <- ec
		} else {
			pw.revocationWaiter.Done() // in the rare occasion this is a stale evetn, decrement the revocation waiter
		}
	}
}

func (pw *partitionWorker[T]) work(interjections []interjection[T], waiter func(), commitLog *eosCommitLog) {
	elapsed := sincer{time.Now()}
	// don't start consuming until this function returns
	// this function will block until all changelogs for this partition are populated
	pw.highestOffset = commitLog.lastProcessed(pw.topicPartition)
	log.Debugf("partitionWorker initialized %+v with lastProcessed offset: %d in %v", pw.topicPartition, pw.highestOffset, elapsed)
	waiter()
	go pw.pushRecords()
	log.Debugf("partitionWorker activated %+v in %v, interjectionCount: %d", pw.topicPartition, elapsed, len(interjections))
	ijPtrs := sak.ToPtrSlice(interjections)
	for _, ij := range ijPtrs {
		ij.init(pw.topicPartition, pw.interjectionChannel)
		ij.tick()
	}
	pw.eventSource.source.onPartitionActivated(pw.topicPartition.Partition)
	for {
		select {
		case ec := <-pw.eventInput:
			// if ec != nil {
			// ec := newEventContext(pw.runStatus.Ctx(), record, pw.changeLog.changeLogData(), pw)
			pw.handleEvent(ec)
			// }
		case job := <-pw.asyncCompleter.asyncJobs:
			// TODO: if the partition was reject and we have not tried to produce yet
			// drop this event. This is tricky because we need to know if we are buffered or not
			if state, _ := job.finalize(); state == Complete {
				job.ctx.complete()
			}
			select {
			case pw.asyncCompleter.asyncFullReply <- struct{}{}:
			default:
			}
		case ij := <-pw.interjectionChannel:
			pw.handleInterjection(ij)
			ij.tick()
		case <-pw.stopSignal:
			for _, ij := range ijPtrs {
				ij.cancel()
			}
			go pw.waitForRevocation()
		case <-pw.revokedSignal:
			pw.stopped <- struct{}{}
			return
		}
	}
}

func (pw *partitionWorker[T]) waitForRevocation() {
	pw.revocationWaiter.Wait() // wait until all pending events have been accpted by a producerNode
	pw.revokedSignal <- struct{}{}
}

type asyncCompleter[T any] struct {
	asyncJobs      chan asyncJob[T]
	asyncFullReply chan struct{}
}

type asyncJob[T any] struct {
	ctx      *EventContext[T]
	finalize func() (ExecutionState, error)
}

func (ac asyncCompleter[T]) asyncComplete(j asyncJob[T]) {
	for {
		select {
		case ac.asyncJobs <- j:
			return
		default:
			log.Tracef("Async completion channel full, you're incoming events are significantly outpacing your async processes.")
			<-ac.asyncFullReply
		}
	}
}

func (pw *partitionWorker[T]) isRevoked() bool {
	return !pw.runStatus.Running()
}

func (pw *partitionWorker[T]) handleInterjection(inter *interjection[T]) {
	// // TODO: how to cancel one off interjections during a revocation?
	if pw.isRevoked() {
		if inter.callback != nil {
			inter.callback()
		}
		return
	}
	pw.revocationWaiter.Add(1)
	ec := newInterjectionContext(pw.runStatus.Ctx(), pw.topicPartition, pw.changeLog.changeLogData(), pw)
	pw.eosProducer.addInterjection(ec)
	ec.producer = <-ec.producerChan
	if ec.producer == nil && inter.callback != nil {
		inter.callback() // we need to close off 1-off interjections to prevent sourceConsumer from hanging
	} else if ec.producer != nil && inter.interject(ec) == Complete {
		ec.complete()
	}
}

func (pw *partitionWorker[T]) handleEvent(ec *EventContext[T]) bool {
	offset := ec.Offset()
	atomic.AddInt64(&pw.pending, -1)
	pw.forwardToEventSource(ec)
	pw.highestOffset = offset + 1
	atomic.AddInt64(&pw.processed, 1)
	return true
}

func (pw *partitionWorker[T]) forwardToEventSource(ec *EventContext[T]) {
	ec.producer = <-ec.producerChan
	if ec.producer == nil {
		// if we're revoked, don't even add this to the onDeck producer
		return
	}
	record, _ := ec.Input()
	state, err := pw.eventSource.handleEvent(ec, record)

	if err != nil {
		log.Errorf("%v", err)
	} else if state == Complete {
		ec.complete()
	}
}
