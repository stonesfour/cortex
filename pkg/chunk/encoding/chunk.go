// This file was taken from Prometheus (https://github.com/prometheus/prometheus).
// The original license header is included below:
//
// Copyright 2014 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package encoding

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/prometheus/common/model"

	"github.com/cortexproject/cortex/pkg/prom1/storage/metric"
)

// ChunkLen is the length of a chunk in bytes.
const ChunkLen = 1024

// DefaultEncoding can be changed via a flag.
var DefaultEncoding = DoubleDelta

var (
	errChunkBoundsExceeded = errors.New("attempted access outside of chunk boundaries")
	errAddedToEvictedChunk = errors.New("attempted to add sample to evicted chunk")
)

// Encoding defines which encoding we are using, delta, doubledelta, or varbit
type Encoding byte

// String implements flag.Value.
func (e Encoding) String() string {
	return fmt.Sprintf("%d", e)
}

// Set implements flag.Value.
func (e *Encoding) Set(s string) error {
	switch s {
	case "0":
		*e = Delta
	case "1":
		*e = DoubleDelta
	case "2":
		*e = Varbit
	case "3":
		*e = Bigchunk
	default:
		return fmt.Errorf("invalid chunk encoding: %s", s)
	}
	return nil
}

const (
	// Delta encoding
	Delta Encoding = iota
	// DoubleDelta encoding
	DoubleDelta
	// Varbit encoding
	Varbit
	// Bigchunk encoding
	Bigchunk
)

// Chunk is the interface for all chunks. Chunks are generally not
// goroutine-safe.
type Chunk interface {
	// Add adds a SamplePair to the chunks, performs any necessary
	// re-encoding, and adds any necessary overflow chunks. It returns the
	// new version of the original chunk, followed by overflow chunks, if
	// any. The first chunk returned might be the same as the original one
	// or a newly allocated version. In any case, take the returned chunk as
	// the relevant one and discard the original chunk.
	Add(sample model.SamplePair) ([]Chunk, error)
	NewIterator() Iterator
	Marshal(io.Writer) error
	UnmarshalFromBuf([]byte) error
	Encoding() Encoding
	Utilization() float64

	// Slice returns a smaller chunk the includes all samples between start and end
	// (inclusive).  Its may over estimate. On some encodings it is a noop.
	Slice(start, end model.Time) Chunk

	// Len returns the number of samples in the chunk.  Implementations may be
	// expensive.
	Len() int

	// Size returns the approximate length of the chunk in bytes.
	Size() int
}

// Iterator enables efficient access to the content of a chunk. It is
// generally not safe to use an Iterator concurrently with or after chunk
// mutation.
type Iterator interface {
	// Scans the next value in the chunk. Directly after the iterator has
	// been created, the next value is the first value in the
	// chunk. Otherwise, it is the value following the last value scanned or
	// found (by one of the Find... methods). Returns false if either the
	// end of the chunk is reached or an error has occurred.
	Scan() bool
	// Finds the oldest value at or after the provided time. Returns false
	// if either the chunk contains no value at or after the provided time,
	// or an error has occurred.
	FindAtOrAfter(model.Time) bool
	// Returns the last value scanned (by the scan method) or found (by one
	// of the find... methods). It returns model.ZeroSamplePair before any of
	// those methods were called.
	Value() model.SamplePair
	// Returns a batch of the provisded size; NB not idempotent!  Should only be called
	// once per Scan.
	Batch(size int) Batch
	// Returns the last error encountered. In general, an error signals data
	// corruption in the chunk and requires quarantining.
	Err() error
}

// BatchSize is samples per batch; this was choose by benchmarking all sizes from
// 1 to 128.
const BatchSize = 12

// Batch is a sorted set of (timestamp, value) pairs.  They are intended to be
// small, and passed by value.
type Batch struct {
	Timestamps [BatchSize]int64
	Values     [BatchSize]float64
	Index      int
	Length     int
}

// RangeValues is a utility function that retrieves all values within the given
// range from an Iterator.
func RangeValues(it Iterator, in metric.Interval) ([]model.SamplePair, error) {
	result := []model.SamplePair{}
	if !it.FindAtOrAfter(in.OldestInclusive) {
		return result, it.Err()
	}
	for !it.Value().Timestamp.After(in.NewestInclusive) {
		result = append(result, it.Value())
		if !it.Scan() {
			break
		}
	}
	return result, it.Err()
}

// addToOverflowChunk is a utility function that creates a new chunk as overflow
// chunk, adds the provided sample to it, and returns a chunk slice containing
// the provided old chunk followed by the new overflow chunk.
func addToOverflowChunk(c Chunk, s model.SamplePair) ([]Chunk, error) {
	overflowChunks, err := New().Add(s)
	if err != nil {
		return nil, err
	}
	return []Chunk{c, overflowChunks[0]}, nil
}

// transcodeAndAdd is a utility function that transcodes the dst chunk into the
// provided src chunk (plus the necessary overflow chunks) and then adds the
// provided sample. It returns the new chunks (transcoded plus overflow) with
// the new sample at the end.
func transcodeAndAdd(dst Chunk, src Chunk, s model.SamplePair) ([]Chunk, error) {
	Ops.WithLabelValues(Transcode).Inc()

	var (
		head            = dst
		body, NewChunks []Chunk
		err             error
	)

	it := src.NewIterator()
	for it.Scan() {
		if NewChunks, err = head.Add(it.Value()); err != nil {
			return nil, err
		}
		body = append(body, NewChunks[:len(NewChunks)-1]...)
		head = NewChunks[len(NewChunks)-1]
	}
	if it.Err() != nil {
		return nil, it.Err()
	}

	if NewChunks, err = head.Add(s); err != nil {
		return nil, err
	}
	return append(body, NewChunks...), nil
}

// New creates a new chunk according to the encoding set by the
// DefaultEncoding flag.
func New() Chunk {
	chunk, err := NewForEncoding(DefaultEncoding)
	if err != nil {
		panic(err)
	}
	return chunk
}

// NewForEncoding allows configuring what chunk type you want
func NewForEncoding(encoding Encoding) (Chunk, error) {
	switch encoding {
	case Delta:
		return newDeltaEncodedChunk(d1, d0, true, ChunkLen), nil
	case DoubleDelta:
		return newDoubleDeltaEncodedChunk(d1, d0, true, ChunkLen), nil
	case Varbit:
		return newVarbitChunk(varbitZeroEncoding), nil
	case Bigchunk:
		return newBigchunk(), nil
	default:
		return nil, fmt.Errorf("unknown chunk encoding: %v", encoding)
	}
}

// indexAccessor allows accesses to samples by index.
type indexAccessor interface {
	timestampAtIndex(int) model.Time
	sampleValueAtIndex(int) model.SampleValue
	err() error
}

// indexAccessingChunkIterator is a chunk iterator for chunks for which an
// indexAccessor implementation exists.
type indexAccessingChunkIterator struct {
	len       int
	pos       int
	lastValue model.SamplePair
	acc       indexAccessor
}

func newIndexAccessingChunkIterator(len int, acc indexAccessor) *indexAccessingChunkIterator {
	return &indexAccessingChunkIterator{
		len:       len,
		pos:       -1,
		lastValue: model.ZeroSamplePair,
		acc:       acc,
	}
}

// scan implements Iterator.
func (it *indexAccessingChunkIterator) Scan() bool {
	it.pos++
	if it.pos >= it.len {
		return false
	}
	it.lastValue = model.SamplePair{
		Timestamp: it.acc.timestampAtIndex(it.pos),
		Value:     it.acc.sampleValueAtIndex(it.pos),
	}
	return it.acc.err() == nil
}

// findAtOrAfter implements Iterator.
func (it *indexAccessingChunkIterator) FindAtOrAfter(t model.Time) bool {
	i := sort.Search(it.len, func(i int) bool {
		return !it.acc.timestampAtIndex(i).Before(t)
	})
	if i == it.len || it.acc.err() != nil {
		return false
	}
	it.pos = i
	it.lastValue = model.SamplePair{
		Timestamp: it.acc.timestampAtIndex(i),
		Value:     it.acc.sampleValueAtIndex(i),
	}
	return true
}

// value implements Iterator.
func (it *indexAccessingChunkIterator) Value() model.SamplePair {
	return it.lastValue
}

func (it *indexAccessingChunkIterator) Batch(size int) Batch {
	var batch Batch
	j := 0
	for j < size && it.pos < it.len {
		batch.Timestamps[j] = int64(it.acc.timestampAtIndex(it.pos))
		batch.Values[j] = float64(it.acc.sampleValueAtIndex(it.pos))
		it.pos++
		j++
	}
	// Interface contract is that you call Scan before calling Batch; therefore
	// without this decrement, you'd end up skipping samples.
	it.pos--
	batch.Index = 0
	batch.Length = j
	return batch
}

// err implements Iterator.
func (it *indexAccessingChunkIterator) Err() error {
	return it.acc.err()
}
