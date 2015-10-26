// Copyright 2015 The Cockroach Authors.
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
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Vivek Menezes (vivek@cockroachlabs.com)

package sql

import (
	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/config"
	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/stop"
	"github.com/gogo/protobuf/proto"
)

// TableLeaderNotification used to return table leader
// notifications from the node.
type TableLeaderNotification struct {
	Key      roachpb.Key
	IsLeader bool
}

// TableLeaderInterface used to register/unregister for table leader
// notifications from a node. A node is a table leader if it is the leader
// for the range containing the table prefix key.
type TableLeaderInterface interface {
	RegisterNotifyTableLeader(key roachpb.Key, s *stop.Stopper, report chan<- TableLeaderNotification)
	UnregisterNotifyTableLeader(key roachpb.Key)
}

// TableLeaderCreator create a table leader responsible for executing schema
// changes on a table.
type TableLeaderCreator struct {
	newConfig chan config.SystemConfig
}

// updateSystemConfig is called whenever the system config gossip entry is updated.
func (t *TableLeaderCreator) updateSystemConfig(cfg *config.SystemConfig) {
	t.newConfig <- *cfg
}

type descriptorState struct {
	descriptor TableDescriptor
	// This node is the leader for this table.
	isLeader bool
	// A schema change routine is running.
	isWorking bool
	// There are no outstanding mutations on this table.
	isDeleted bool
}

// Completed work on a table.
type mutationDone struct {
	key roachpb.Key
}

// Start runs a loop that looks for table mutations in system configuration changes.
// It registers with node for every table with an outstanding mutation. Under the
// circumstances that it is a table leader for a table, it applies the mutations on
// the table.
func (t TableLeaderCreator) Start(s *stop.Stopper, gossip *gossip.Gossip, node TableLeaderInterface, db *client.DB) {
	s.RunWorker(func() {
		// Create channel that receives new system configs.
		t.newConfig = make(chan config.SystemConfig)
		gossip.RegisterSystemConfigCallback(t.updateSystemConfig)
		// Create channel that receives table leader notifications.
		ch := make(chan TableLeaderNotification)
		// Create channel that receives table mutation completion notifications.
		done := make(chan mutationDone)
		// All the table descriptors with outstanding mutations.
		tables := make(map[string]descriptorState)
		// The names of all the databases. We have to recreate this because some of
		// the sql operations used in running the mutations need full table names:
		// parser.QualifiedName.
		databases := make(map[ID]string)

		for {
			select {
			case cfg := <-t.newConfig:
				log.Info("received a new config")
				// Read config; attempt to create table leader to run
				// outstanding mutations.

				// Existing tables being followed.
				delTables := make(map[string]struct{})
				for k := range tables {
					delTables[k] = struct{}{}
				}

				for _, kv := range cfg.Values {
					// Attempt to unmarshal config into a table descriptor.
					var descriptor TableDescriptor
					if err := proto.Unmarshal(kv.Value.RawBytes, &descriptor); err != nil {
						// Attempt to unmarshal config into a database descriptor.
						var dbDescriptor DatabaseDescriptor
						if err := proto.Unmarshal(kv.Value.RawBytes, &dbDescriptor); err == nil {
							databases[dbDescriptor.ID] = dbDescriptor.Name
						}
						continue
					}
					// Unmarshal successful; found a table descriptor.
					if err := descriptor.Validate(); err != nil {
						log.Errorf("received invalid table descriptor: %v", descriptor)
						continue
					}
					if len(descriptor.Mutations) > 0 {
						key := MakeIndexKeyPrefix(descriptor.ID, descriptor.PrimaryIndex.ID)
						keyString := string(key)
						d, ok := tables[keyString]
						if !ok {
							d = descriptorState{descriptor: descriptor}
							tables[keyString] = d
							// Register a key for the first range of the table data for notifications.
							node.RegisterNotifyTableLeader(key, s, ch)
						}
						// Keep the key in tables. Remove it from delTables
						delete(delTables, keyString)
						// Possibly apply mutation.
						if d.isLeader && !d.isWorking {
							d.isWorking = true
							tables[keyString] = d
							go func() {
								if err := applyOneMutation(db, d.descriptor, databases[d.descriptor.ParentID]); err != nil {
									log.Info(err)
								}
								done <- mutationDone{key: roachpb.Key(key)}
							}()
						}
					}
				}
				// Unregister all the tables in delTables.
				for key := range delTables {
					if _, ok := tables[key]; ok {
						if !tables[key].isWorking {
							node.UnregisterNotifyTableLeader(roachpb.Key(key))
							delete(tables, key)
						} else {
							d := tables[key]
							d.isDeleted = true
							tables[key] = d
						}
					}
				}

			case n := <-ch:
				log.Info("received a leader notification")
				keyString := string(n.Key)
				if d, ok := tables[keyString]; ok {
					if n.IsLeader && !d.isLeader {
						d.isLeader = true
						d.isWorking = true
						tables[keyString] = d
						// start schema change  goroutine.
						go func() {
							descriptor := tables[keyString].descriptor
							if err := applyOneMutation(db, descriptor, databases[descriptor.ParentID]); err != nil {
								log.Info(err)
							}
							done <- mutationDone{key: n.Key}
						}()
					} else if !n.IsLeader && d.isLeader {
						// Notify table leader to stop.
						d.isLeader = false
						tables[keyString] = d
					}
				} else {
					log.Info("received leader notification for table not followed")
				}

			case d := <-done:
				keyString := string(d.key)
				if v, ok := tables[keyString]; ok {
					if !v.isWorking {
						panic("received work completion notification with no pending work")
					}
					v.isWorking = false
					// Continue to work on the next mutation if present when a new system
					// config is read.
					tables[keyString] = v
					if v.isDeleted {
						node.UnregisterNotifyTableLeader(roachpb.Key(keyString))
						delete(tables, keyString)
					}
				} else {
					panic("received work completion notification for table not followed")
				}

			case <-s.ShouldStop():
				return
			}
		}
	})
}
