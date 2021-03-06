// Copyright 2016 The etcd Authors
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

package grpcproxy

import (
	"sync"

	"github.com/coreos/etcd/clientv3"
	pb "github.com/coreos/etcd/etcdserver/etcdserverpb"

	"golang.org/x/net/context"
)

type watchergroups struct {
	cw clientv3.Watcher

	mu        sync.Mutex
	groups    map[watchRange]*watcherGroup
	idToGroup map[receiverID]*watcherGroup

	proxyCtx context.Context
}

func (wgs *watchergroups) addWatcher(rid receiverID, w watcher) {
	wgs.mu.Lock()
	defer wgs.mu.Unlock()

	groups := wgs.groups

	if wg, ok := groups[w.wr]; ok {
		rev := wg.add(rid, w)
		wgs.idToGroup[rid] = wg

		if rev == 0 {
			// The group is newly created, the create event has not been delivered
			// to this group yet.
			// We can rely on etcd server to deliver the create event.
			// Or we might end up sending created event twice.
			return
		}

		resp := &pb.WatchResponse{
			Header: &pb.ResponseHeader{
				// todo: fill in ClusterId
				// todo: fill in MemberId:
				Revision: rev,
				// todo: fill in RaftTerm:
			},
			WatchId: rid.watcherID,
			Created: true,
		}
		select {
		case w.sws.watchCh <- resp:
		case <-w.sws.ctx.Done():
		}
		return
	}

	ctx, cancel := context.WithCancel(wgs.proxyCtx)

	wch := wgs.cw.Watch(ctx, w.wr.key,
		clientv3.WithRange(w.wr.end),
		clientv3.WithProgressNotify(),
		clientv3.WithCreatedNotify(),
	)

	watchg := newWatchergroup(wch, cancel)
	watchg.add(rid, w)
	go watchg.run()
	groups[w.wr] = watchg
	wgs.idToGroup[rid] = watchg
}

func (wgs *watchergroups) removeWatcher(rid receiverID) (int64, bool) {
	wgs.mu.Lock()
	defer wgs.mu.Unlock()

	if g, ok := wgs.idToGroup[rid]; ok {
		g.delete(rid)
		delete(wgs.idToGroup, rid)
		if g.isEmpty() {
			g.stop()
		}
		return g.revision(), true
	}
	return -1, false
}

func (wgs *watchergroups) maybeJoinWatcherSingle(ws watcherSingle) bool {
	wgs.mu.Lock()
	defer wgs.mu.Unlock()

	rid := receiverID{streamID: ws.sws.id, watcherID: ws.w.id}
	group, ok := wgs.groups[ws.w.wr]
	if ok {
		return group.add(rid, ws.w) != -1
	}
	if !ws.canPromote() {
		return false
	}
	wg := newWatchergroup(ws.ch, ws.cancel)
	wgs.groups[ws.w.wr] = wg
	wgs.idToGroup[rid] = wg
	wg.add(rid, ws.w)
	go wg.run()
	return true
}

func (wgs *watchergroups) stop() {
	wgs.mu.Lock()
	defer wgs.mu.Unlock()
	for _, wg := range wgs.groups {
		wg.stop()
	}
	wgs.groups = nil
}
