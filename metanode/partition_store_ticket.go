// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package metanode

import (
	"encoding/binary"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/cmd/common"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
)

type storeMsg struct {
	command      uint32
	snap         Snapshot
	quotaRebuild bool
	uidRebuild   bool
	uniqId       uint64
	uniqChecker  *uniqChecker
	multiVerList []*proto.VolVersionInfo
}

func (mp *metaPartition) startSchedule(curIndex uint64) {
	timer := time.NewTimer(time.Hour * 24 * 365)
	timer.Stop()
	timerCursor := time.NewTimer(intervalToSyncCursor)
	scheduleState := common.StateStopped
	lastCursor := mp.GetCursor()
	dumpFunc := func(msg *storeMsg) {
		log.LogWarnf("[startSchedule] partitionId=%d: nowAppID"+
			"=%d, applyID=%d", mp.config.PartitionId, curIndex,
			msg.snap.ApplyID())
		if err := mp.store(msg); err == nil {
			// truncate raft log
			if mp.raftPartition != nil {
				log.LogWarnf("[startSchedule] start trunc, partitionId=%d: nowAppID"+
					"=%d, applyID=%d", mp.config.PartitionId, curIndex,
					msg.snap.ApplyID())
				mp.raftPartition.Truncate(curIndex)
			} else {
				// maybe happen when start load dentry
				log.LogWarnf("[startSchedule] raftPartition is nil so skip" +
					" truncate raft log")
			}
			curIndex = msg.snap.ApplyID()
		} else {
			// retry again
			mp.storeChan <- msg
			err = errors.NewErrorf("[startSchedule]: dump partition id=%d: %v",
				mp.config.PartitionId, err.Error())
			log.LogErrorf(err.Error())
			exporter.Warning(err.Error())
		}

		if _, ok := mp.IsLeader(); ok {
			timer.Reset(intervalToPersistData)
		}
		atomic.StoreUint32(&scheduleState, common.StateStopped)
	}
	go func(stopC chan bool) {
		var msgs []*storeMsg
		readyChan := make(chan struct{}, 1)
		for {
			if len(msgs) > 0 {
				if atomic.CompareAndSwapUint32(&scheduleState, common.StateStopped, common.StateRunning) {
					readyChan <- struct{}{}
				}
			}
			select {
			case <-stopC:
				timer.Stop()
				return

			case <-readyChan:
				var (
					maxIdx uint64
					maxMsg *storeMsg
				)
				for _, msg := range msgs {
					if curIndex >= msg.snap.ApplyID() {
						if msg.snap != nil {
							msg.snap.Close()
						}
						continue
					}
					if maxIdx < msg.snap.ApplyID() {
						if maxMsg != nil && maxMsg.snap != nil {
							maxMsg.snap.Close()
						}
						maxIdx = msg.snap.ApplyID()
						maxMsg = msg
					} else {
						if msg.snap != nil {
							msg.snap.Close()
						}
					}
				}
				if maxMsg != nil {
					go dumpFunc(maxMsg)
				} else {
					atomic.StoreUint32(&scheduleState, common.StateStopped)
				}
				msgs = msgs[:0]
			case msg := <-mp.storeChan:
				switch msg.command {
				case startStoreTick:
					timer.Reset(intervalToPersistData)
				case stopStoreTick:
					timer.Stop()
				case opFSMStoreTick:
					msgs = append(msgs, msg)
				default:
					// do nothing
				}
			case <-timer.C:
				log.LogDebugf("[startSchedule] intervalToPersistData curIndex: %v,apply:%v", curIndex, mp.applyID)
				if mp.applyID <= curIndex {
					timer.Reset(intervalToPersistData)
					continue
				}
				if _, err := mp.submit(opFSMStoreTick, nil); err != nil {
					log.LogErrorf("[startSchedule] raft submit: %s", err.Error())
					if _, ok := mp.IsLeader(); ok {
						timer.Reset(intervalToPersistData)
					}
				}
			case <-timerCursor.C:
				if _, ok := mp.IsLeader(); !ok {
					timerCursor.Reset(intervalToSyncCursor)
					continue
				}
				curCursor := mp.GetCursor()
				if curCursor == lastCursor {
					log.LogDebugf("[startSchedule] partitionId=%d: curCursor[%v]=lastCursor[%v]",
						mp.config.PartitionId, curCursor, lastCursor)
					timerCursor.Reset(intervalToSyncCursor)
					continue
				}
				Buf := make([]byte, 8)
				binary.BigEndian.PutUint64(Buf, curCursor)
				if _, err := mp.submit(opFSMSyncCursor, Buf); err != nil {
					log.LogErrorf("[startSchedule] raft submit: %s", err.Error())
				}

				binary.BigEndian.PutUint64(Buf, mp.txProcessor.txManager.txIdAlloc.getTransactionID())
				if _, err := mp.submit(opFSMSyncTxID, Buf); err != nil {
					log.LogErrorf("[startSchedule] raft submit: %s", err.Error())
				}
				lastCursor = curCursor
				timerCursor.Reset(intervalToSyncCursor)
			}
		}
	}(mp.stopC)
}

func (mp *metaPartition) stop() {
	if mp.stopC != nil {
		close(mp.stopC)
	}
}
