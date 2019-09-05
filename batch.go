package tracedb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

const (
	batchHeaderLen = 8 + 4
	batchGrowRec   = 3000
	// batchBufioSize = 16

	// Maximum value possible for sequence number; the 8-bits are
	// used by value type, so its can packed together in single
	// 64-bit integer.
	keyMaxSeq = (uint64(1) << 56) - 1
	// Maximum value possible for packed sequence number and type.
	keyMaxNum = (keyMaxSeq << 8) | 0
)

type batchIndex struct {
	delFlag   bool
	hash      uint32
	keySize   uint16
	valueSize uint32
	expiresAt uint32
	kvOffset  int
}

func (index batchIndex) k(data []byte) []byte {
	return data[index.kvOffset : index.kvOffset+int(index.keySize)]
}

func (index batchIndex) kvSize() uint32 {
	return uint32(index.keySize) + index.valueSize
}

func (index batchIndex) kv(data []byte) (key, value []byte) {
	keyValue := data[index.kvOffset : index.kvOffset+int(index.kvSize())]
	return keyValue[:index.keySize], keyValue[index.keySize:]

}

type internalKey []byte

func makeInternalKey(dst, ukey []byte, seq uint64, dFlag bool, expiresAt uint32) internalKey {
	if seq > keyMaxSeq {
		panic("tracedb: invalid sequence number")
	}

	var dBit int8
	if dFlag {
		dBit = 1
	}
	dst = ensureBuffer(dst, len(ukey)+12)
	copy(dst, ukey)
	binary.LittleEndian.PutUint64(dst[len(ukey):len(ukey)+8], (seq<<8)|uint64(dBit))
	binary.LittleEndian.PutUint32(dst[len(ukey)+8:], expiresAt)
	return internalKey(dst)
}

func parseInternalKey(ik []byte) (ukey []byte, seq uint64, dFlag bool, expiresAt uint32, err error) {
	if len(ik) < 12 {
		logger.Print("invalid internal key length")
		return
	}
	expiresAt = binary.LittleEndian.Uint32(ik[len(ik)-4:])
	num := binary.LittleEndian.Uint64(ik[len(ik)-12 : len(ik)-4])
	seq, dFlag = uint64(num>>8), num&0xff != 0
	ukey = ik[:len(ik)-12]
	return
}

// Batch is a write batch.
type Batch struct {
	managed      bool
	batchSeq     uint64
	db           *DB
	data         []byte
	index        []batchIndex
	firstKeyHash uint32

	//Batch memdb
	mem *memdb

	// internalLen is sums of key/value pair length plus 8-bytes internal key.
	internalLen uint32
}

// init initializes the batch.
func (b *Batch) init(db *DB) error {
	if b.mem != nil {
		panic("tracedb: batch is inprogress")
	}
	b.db = db
	if db.mem.getref() == 0 {
		db.mem = db.mpoolGet(0)
	}
	b.mem = db.mem
	b.mem.incref()
	return nil
}

func (b *Batch) grow(n int) {
	o := len(b.data)
	if cap(b.data)-o < n {
		div := 1
		if len(b.index) > batchGrowRec {
			div = len(b.index) / batchGrowRec
		}
		ndata := make([]byte, o, o+n+o/div)
		copy(ndata, b.data)
		b.data = ndata
	}
}

func (b *Batch) appendRec(dFlag bool, expiresAt uint32, key, value []byte) {
	n := 1 + len(key)
	if !dFlag {
		n += len(value)
	}
	b.grow(n)
	index := batchIndex{delFlag: dFlag, hash: b.mem.hash(key), keySize: uint16(len(key))}
	o := len(b.data)
	data := b.data[:o+n]
	if dFlag {
		data[o] = 1
	} else {
		data[o] = 0
	}
	o++
	index.kvOffset = o
	o += copy(data[o:], key)
	if !dFlag {
		index.valueSize = uint32(len(value))
		o += copy(data[o:], value)
	}
	b.data = data[:o]
	index.expiresAt = expiresAt
	b.index = append(b.index, index)
	b.internalLen += uint32(index.keySize) + index.valueSize + 8
}

func (b *Batch) mput(dFlag bool, h uint32, expiresAt uint32, key, value []byte) error {
	switch {
	case len(key) == 0:
		return errKeyEmpty
	case len(key) > MaxKeyLength:
		return errKeyTooLarge
	case len(value) > MaxValueLength:
		return errValueTooLarge
	}

	var k []byte
	k = makeInternalKey(k, key, b.mem.seq+1, dFlag, expiresAt)
	if err := b.mem.put(h, k, value, expiresAt); err != nil {
		return err
	}
	if float64(b.mem.count)/float64(b.mem.nBuckets*entriesPerBucket) > loadFactor {
		if err := b.mem.split(); err != nil {
			return err
		}
	}
	if b.firstKeyHash == 0 {
		b.firstKeyHash = h
	}
	b.mem.seq++
	return nil
}

// Put appends 'put operation' of the given key/value pair to the batch.
// It is safe to modify the contents of the argument after Put returns but not
// before.
func (b *Batch) Put(key, value []byte) {
	b.PutWithTTL(key, value, 0)
}

// PutWithTTL appends 'put operation' of the given key/value pair to the batch and add key expiry time.
// It is safe to modify the contents of the argument after Put returns but not
// before.
func (b *Batch) PutWithTTL(key, value []byte, ttl time.Duration) {
	var expiresAt uint32
	if ttl != 0 {
		expiresAt = uint32(time.Now().Add(ttl).Unix())
	}
	b.appendRec(false, expiresAt, key, value)
}

// Delete appends 'delete operation' of the given key to the batch.
// It is safe to modify the contents of the argument after Delete returns but
// not before.
func (b *Batch) Delete(key []byte) {
	var expiresAt uint32
	b.appendRec(true, expiresAt, key, nil)
}

func (b *Batch) writeInternal(fn func(i int, dFlag bool, h uint32, expiresAt uint32, k, v []byte) error) error {
	start := time.Now()
	defer logger.Print("batch.Write: ", time.Since(start))
	pendingWrites := b.uniq()
	for i, index := range pendingWrites {
		key, val := index.kv(b.data)
		if err := fn(i, index.delFlag, index.hash, index.expiresAt, key, val); err != nil {
			return err
		}
	}
	return nil
}

// Write apply the given batch to the transaction. The batch will be applied
// sequentially.
// Please note that the transaction is not compacted until committed, so if you
// writes 10 same keys, then those 10 same keys are in the transaction.
//
// It is safe to modify the contents of the arguments after Write returns.
func (b *Batch) Write() error {
	// The write happen synchronously.
	b.db.writeLockC <- struct{}{}
	b.batchSeq = b.mem.seq
	return b.writeInternal(func(i int, dFlag bool, h uint32, expiresAt uint32, k, v []byte) error {
		return b.mput(dFlag, h, expiresAt, k, v)
	})
}

func (b *Batch) commit() error {
	var delCount int64 = 0
	var putCount int64 = 0
	var bh *bucketHandle
	var originalB *bucketHandle
	entryIdx := 0
	b.db.mu.Lock()
	defer func() {
		b.db.mu.Unlock()
	}()
	bucketIdx := b.mem.bucketIndex(b.firstKeyHash)
	for bucketIdx < b.mem.nBuckets {
		err := b.mem.forEachBucket(bucketIdx, func(memb bucketHandle) (bool, error) {
			for i := 0; i < entriesPerBucket; i++ {
				memsl := memb.entries[i]
				if memsl.kvOffset == 0 {
					return memb.next == 0, nil
				}
				memslKey, value, err := b.mem.data.readKeyValue(memsl)
				if err == errKeyExpired {
					continue
				}
				if err != nil {
					return true, err
				}
				key, seq, dFlag, expiresAt, err := parseInternalKey(memslKey)
				if err != nil {
					return true, err
				}
				if seq <= b.batchSeq {
					continue
				}
				if seq > b.batchSeq+uint64(b.Len()) {
					return true, errBatchSeqComplete
				}
				hash := b.db.hash(key)

				if dFlag {
					/// Test filter block for presence
					if !b.db.filter.Test(uint64(hash)) {
						return false, nil
					}
					delCount++
					bh := bucketHandle{}
					delentryIdx := -1
					err = b.db.forEachBucket(b.db.bucketIndex(hash), func(curb bucketHandle) (bool, error) {
						bh = curb
						for i := 0; i < entriesPerBucket; i++ {
							sl := bh.entries[i]
							if sl.kvOffset == 0 {
								return bh.next == 0, nil
							} else if hash == sl.hash && uint16(len(key)) == sl.keySize {
								slKey, err := b.db.data.readKey(sl)
								if err != nil {
									return true, err
								}
								if bytes.Equal(key, slKey) {
									delentryIdx = i
									return true, nil
								}
							}
						}
						return false, nil
					})
					if delentryIdx == -1 || err != nil {
						return false, err
					}
					sl := bh.entries[delentryIdx]
					bh.del(delentryIdx)
					if err := bh.write(); err != nil {
						return false, err
					}
					b.db.data.free(sl.kvSize(), sl.kvOffset)
					b.db.count--
				} else {
					putCount++
					err = b.db.forEachBucket(b.db.bucketIndex(hash), func(curb bucketHandle) (bool, error) {
						bh = &curb
						for i := 0; i < entriesPerBucket; i++ {
							sl := bh.entries[i]
							entryIdx = i
							if sl.kvOffset == 0 {
								// Found an empty entry.
								return true, nil
							} else if hash == sl.hash && uint16(len(key)) == sl.keySize {
								// Key already exists.
								if slKey, err := b.db.data.readKey(sl); bytes.Equal(key, slKey) || err != nil {
									return true, err
								}
							}
						}
						if bh.next == 0 {
							// Couldn't find free space in the current bucketHandle, creating a new overflow bucketHandle.
							nextBucket, err := b.db.createOverflowBucket()
							if err != nil {
								return false, err
							}
							bh.next = nextBucket.offset
							originalB = bh
							bh = nextBucket
							entryIdx = 0
							return true, nil
						}
						return false, nil
					})

					if err != nil {
						return false, err
					}
					// Inserting a new item.
					if bh.entries[entryIdx].kvOffset == 0 {
						if b.db.count == MaxKeys {
							return false, errFull
						}
						b.db.count++
					} else {
						defer b.db.data.free(bh.entries[entryIdx].kvSize(), bh.entries[entryIdx].kvOffset)
					}

					bh.entries[entryIdx] = entry{
						hash:      hash,
						keySize:   uint16(len(key)),
						valueSize: uint32(len(value)),
						expiresAt: expiresAt,
					}
					if bh.entries[entryIdx].kvOffset, err = b.db.data.writeKeyValue(key, value); err != nil {
						return false, err
					}
					if err := bh.write(); err != nil {
						return false, err
					}
					if originalB != nil {
						if err := originalB.write(); err != nil {
							return false, err
						}
					}
					b.db.filter.Append(uint64(hash))
				}
			}
			return false, nil
		})
		if err == errBatchSeqComplete {
			break
		}
		if err != nil {
			return err
		}
		bucketIdx++
	}
	b.db.metrics.Dels.Add(delCount)
	b.db.metrics.Puts.Add(putCount)

	if b.db.syncWrites {
		return b.db.sync()
	}

	return nil
}

func (b *Batch) Commit() error {
	_assert(!b.managed, "managed tx commit not allowed")
	if b.mem == nil || b.mem.getref() == 0 {
		return nil
	}
	return b.commit()
}

func (b *Batch) Abort() {
	_assert(!b.managed, "managed tx commit not allowed")
	b.Reset()
	b.mem.decref()
	b.mem = nil
	<-b.db.writeLockC
}

// Len returns number of records in the batch.
func (b *Batch) Len() int {
	return len(b.index)
}

// Reset resets the batch.
func (b *Batch) Reset() {
	b.data = b.data[:0]
	b.index = b.index[:0]
	b.internalLen = 0
}

// _assert will panic with a given formatted message if the given condition is false.
func _assert(condition bool, msg string, v ...interface{}) {
	if !condition {
		panic(fmt.Sprintf("assertion failed: "+msg, v...))
	}
}

func (b *Batch) uniq() []batchIndex {
	unique_set := make(map[uint32]int, len(b.index))
	index_set := make(map[uint32]batchIndex, len(b.index))
	i := 0
	for idx := len(b.index) - 1; idx >= 0; idx-- {
		if _, ok := unique_set[b.index[idx].hash]; !ok {
			unique_set[b.index[idx].hash] = i
			index_set[b.index[idx].hash] = b.index[idx]
			i++
		}
	}
	pendingWrites := make([]batchIndex, len(unique_set))
	for x, i := range unique_set {
		pendingWrites[len(unique_set)-i-1] = index_set[x]
	}
	return pendingWrites
}
