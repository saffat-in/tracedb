package tracedb

import (
	"errors"
	"fmt"

	_ "net/http/pprof"

	"github.com/unit-io/tracedb/message"
	"github.com/unit-io/tracedb/wal"
)

func (db *syncHandle) recoverWindowBlocks() error {
	err := db.timeWindow.foreachTimeWindow(true, func(last bool, windowEntries map[uint64]windowEntries) (bool, error) {
		for h, wEntries := range windowEntries {
			topicOff, ok := db.trie.getOffset(h)
			if !ok {
				return true, errors.New("recovery.recoverWindowBlocks error: unable to get topic offset from trie")
			}
			wOff, err := db.windowWriter.append(h, topicOff, wEntries)
			if err != nil {
				return true, err
			}
			if ok := db.trie.setOffset(h, wOff); !ok {
				return true, errors.New("db:Sync: timeWindow sync error: unable to set topic offset in trie")
			}
		}
		return false, nil
	})
	return err
}

func (db *syncHandle) startRecovery() error {
	// p := profile.Start(profile.MemProfile, profile.ProfilePath("."), profile.NoShutdownHook)
	// defer p.Stop()
	db.closeW.Add(1)
	defer func() {
		db.closeW.Done()
	}()
	fmt.Println("db.recoverLog: start recovery")

	if ok := db.startSync(); !ok {
		return nil
	}
	defer func() {
		db.finish()
	}()

	var logEntry entry
	err := db.wal.Read(func(upperSeq uint64, last bool, r *wal.Reader) (ok bool, err error) {
		l := r.Count()
		for i := uint32(0); i < l; i++ {
			logData, ok := r.Next()
			if !ok {
				break
			}
			entryData, data := logData[:entrySize], logData[entrySize:]
			if err := logEntry.UnmarshalBinary(entryData); err != nil {
				return true, err
			}
			msgOffset := logEntry.mSize()
			m := data[:msgOffset]
			if logEntry.msgOffset, err = db.dataWriter.writeMessage(m); err != nil {
				return true, err
			}
			exists, err := db.blockWriter.append(logEntry, db.blocks())
			if err != nil {
				return true, err
			}
			if exists {
				continue
			}
			t := m[int64(idSize) : int64(logEntry.topicSize)+int64(idSize)]

			topic := new(message.Topic)
			if err := topic.Unmarshal(t); err != nil {
				return true, err
			}
			contract := message.Contract(topic.Parts)
			topicHash := topic.GetHash(contract)
			we := winEntry{
				contract: contract,
				seq:      logEntry.seq,
			}
			db.timeWindow.add(topicHash, we)
			if ok := db.trie.addTopic(contract, topicHash, topic.Parts, topic.Depth); !ok {
				return true, errBadRequest
			}
			db.filter.Append(logEntry.seq)
			db.incount()
			db.meter.Recovers.Inc(1)
			db.meter.InMsgs.Inc(1)
			db.meter.InBytes.Inc(int64(logEntry.valueSize))
		}

		if err := db.recoverWindowBlocks(); err != nil {
			return true, err
		}

		if db.upperSeq < upperSeq {
			db.upperSeq = upperSeq
		}

		if last || db.rawData.Size() > db.opts.BufferSize {
			nBlocks := db.blockWriter.Count()
			for i := 0; i < nBlocks; i++ {
				if _, err := db.newBlock(); err != nil {
					return true, err
				}
			}
			if _, err := db.dataWriter.write(); err != nil {
				return true, err
			}
			if err := db.blockWriter.write(); err != nil {
				return true, err
			}
			if err := db.windowWriter.write(); err != nil {
				return true, err
			}
			if err := db.sync(); err != nil {
				return true, err
			}
		}

		return false, nil
	})
	if err != nil {
		return err
	}
	if err := db.wal.SignalLogApplied(db.upperSeq); err != nil {
		return err
	}
	return nil
}

func (db *DB) recoverLog() error {
	// Sync happens synchronously
	db.syncLockC <- struct{}{}
	defer func() {
		<-db.syncLockC
	}()

	syncHandle := syncHandle{DB: db, internal: internal{}}
	return syncHandle.startRecovery()
}
