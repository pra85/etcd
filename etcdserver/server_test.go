// Copyright 2015 CoreOS, Inc.
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

package etcdserver

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	pb "github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/lease"
	"github.com/coreos/etcd/pkg/idutil"
	"github.com/coreos/etcd/pkg/pbutil"
	"github.com/coreos/etcd/pkg/testutil"
	"github.com/coreos/etcd/pkg/types"
	"github.com/coreos/etcd/pkg/wait"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/rafthttp"
	dstorage "github.com/coreos/etcd/storage"
	"github.com/coreos/etcd/storage/backend"
	"github.com/coreos/etcd/store"
)

// TestDoLocalAction tests requests which do not need to go through raft to be applied,
// and are served through local data.
func TestDoLocalAction(t *testing.T) {
	tests := []struct {
		req pb.Request

		wresp    Response
		werr     error
		wactions []testutil.Action
	}{
		{
			pb.Request{Method: "GET", ID: 1, Wait: true},
			Response{Watcher: store.NewNopWatcher()}, nil, []testutil.Action{{Name: "Watch"}},
		},
		{
			pb.Request{Method: "GET", ID: 1},
			Response{Event: &store.Event{}}, nil,
			[]testutil.Action{
				{
					Name:   "Get",
					Params: []interface{}{"", false, false},
				},
			},
		},
		{
			pb.Request{Method: "HEAD", ID: 1},
			Response{Event: &store.Event{}}, nil,
			[]testutil.Action{
				{
					Name:   "Get",
					Params: []interface{}{"", false, false},
				},
			},
		},
		{
			pb.Request{Method: "BADMETHOD", ID: 1},
			Response{}, ErrUnknownMethod, []testutil.Action{},
		},
	}
	for i, tt := range tests {
		st := store.NewRecorder()
		srv := &EtcdServer{
			store:    st,
			reqIDGen: idutil.NewGenerator(0, time.Time{}),
		}
		resp, err := srv.Do(context.TODO(), tt.req)

		if err != tt.werr {
			t.Fatalf("#%d: err = %+v, want %+v", i, err, tt.werr)
		}
		if !reflect.DeepEqual(resp, tt.wresp) {
			t.Errorf("#%d: resp = %+v, want %+v", i, resp, tt.wresp)
		}
		gaction := st.Action()
		if !reflect.DeepEqual(gaction, tt.wactions) {
			t.Errorf("#%d: action = %+v, want %+v", i, gaction, tt.wactions)
		}
	}
}

// TestDoBadLocalAction tests server requests which do not need to go through consensus,
// and return errors when they fetch from local data.
func TestDoBadLocalAction(t *testing.T) {
	storeErr := fmt.Errorf("bah")
	tests := []struct {
		req pb.Request

		wactions []testutil.Action
	}{
		{
			pb.Request{Method: "GET", ID: 1, Wait: true},
			[]testutil.Action{{Name: "Watch"}},
		},
		{
			pb.Request{Method: "GET", ID: 1},
			[]testutil.Action{
				{
					Name:   "Get",
					Params: []interface{}{"", false, false},
				},
			},
		},
		{
			pb.Request{Method: "HEAD", ID: 1},
			[]testutil.Action{
				{
					Name:   "Get",
					Params: []interface{}{"", false, false},
				},
			},
		},
	}
	for i, tt := range tests {
		st := store.NewErrRecorder(storeErr)
		srv := &EtcdServer{
			store:    st,
			reqIDGen: idutil.NewGenerator(0, time.Time{}),
		}
		resp, err := srv.Do(context.Background(), tt.req)

		if err != storeErr {
			t.Fatalf("#%d: err = %+v, want %+v", i, err, storeErr)
		}
		if !reflect.DeepEqual(resp, Response{}) {
			t.Errorf("#%d: resp = %+v, want %+v", i, resp, Response{})
		}
		gaction := st.Action()
		if !reflect.DeepEqual(gaction, tt.wactions) {
			t.Errorf("#%d: action = %+v, want %+v", i, gaction, tt.wactions)
		}
	}
}

// TestApplyRepeat tests that server handles repeat raft messages gracefully
func TestApplyRepeat(t *testing.T) {
	n := newNodeConfChangeCommitterRecorder()
	n.readyc <- raft.Ready{
		SoftState: &raft.SoftState{RaftState: raft.StateLeader},
	}
	cl := newTestCluster(nil)
	st := store.New()
	cl.SetStore(store.New())
	cl.AddMember(&Member{ID: 1234})
	s := &EtcdServer{
		r: raftNode{
			Node:        n,
			raftStorage: raft.NewMemoryStorage(),
			storage:     &storageRecorder{},
			transport:   rafthttp.NewNopTransporter(),
		},
		cfg:      &ServerConfig{},
		store:    st,
		cluster:  cl,
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	s.start()
	req := &pb.Request{Method: "QGET", ID: uint64(1)}
	ents := []raftpb.Entry{{Index: 1, Data: pbutil.MustMarshal(req)}}
	n.readyc <- raft.Ready{CommittedEntries: ents}
	// dup msg
	n.readyc <- raft.Ready{CommittedEntries: ents}

	// use a conf change to block until dup msgs are all processed
	cc := &raftpb.ConfChange{Type: raftpb.ConfChangeRemoveNode, NodeID: 2}
	ents = []raftpb.Entry{{
		Index: 2,
		Type:  raftpb.EntryConfChange,
		Data:  pbutil.MustMarshal(cc),
	}}
	n.readyc <- raft.Ready{CommittedEntries: ents}
	act, err := n.Wait(1)
	s.Stop()

	// only want to confirm etcdserver won't panic; no data to check

	if err != nil {
		t.Fatal(err)
	}
	if len(act) == 0 {
		t.Fatalf("expected len(act)=0, got %d", len(act))
	}
}

func TestApplyRequest(t *testing.T) {
	tests := []struct {
		req pb.Request

		wresp    Response
		wactions []testutil.Action
	}{
		// POST ==> Create
		{
			pb.Request{Method: "POST", ID: 1},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Create",
					Params: []interface{}{"", false, "", true, time.Time{}},
				},
			},
		},
		// POST ==> Create, with expiration
		{
			pb.Request{Method: "POST", ID: 1, Expiration: 1337},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Create",
					Params: []interface{}{"", false, "", true, time.Unix(0, 1337)},
				},
			},
		},
		// POST ==> Create, with dir
		{
			pb.Request{Method: "POST", ID: 1, Dir: true},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Create",
					Params: []interface{}{"", true, "", true, time.Time{}},
				},
			},
		},
		// PUT ==> Set
		{
			pb.Request{Method: "PUT", ID: 1},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Set",
					Params: []interface{}{"", false, "", time.Time{}},
				},
			},
		},
		// PUT ==> Set, with dir
		{
			pb.Request{Method: "PUT", ID: 1, Dir: true},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Set",
					Params: []interface{}{"", true, "", time.Time{}},
				},
			},
		},
		// PUT with PrevExist=true ==> Update
		{
			pb.Request{Method: "PUT", ID: 1, PrevExist: pbutil.Boolp(true)},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Update",
					Params: []interface{}{"", "", time.Time{}},
				},
			},
		},
		// PUT with PrevExist=false ==> Create
		{
			pb.Request{Method: "PUT", ID: 1, PrevExist: pbutil.Boolp(false)},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Create",
					Params: []interface{}{"", false, "", false, time.Time{}},
				},
			},
		},
		// PUT with PrevExist=true *and* PrevIndex set ==> CompareAndSwap
		{
			pb.Request{Method: "PUT", ID: 1, PrevExist: pbutil.Boolp(true), PrevIndex: 1},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "CompareAndSwap",
					Params: []interface{}{"", "", uint64(1), "", time.Time{}},
				},
			},
		},
		// PUT with PrevExist=false *and* PrevIndex set ==> Create
		{
			pb.Request{Method: "PUT", ID: 1, PrevExist: pbutil.Boolp(false), PrevIndex: 1},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Create",
					Params: []interface{}{"", false, "", false, time.Time{}},
				},
			},
		},
		// PUT with PrevIndex set ==> CompareAndSwap
		{
			pb.Request{Method: "PUT", ID: 1, PrevIndex: 1},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "CompareAndSwap",
					Params: []interface{}{"", "", uint64(1), "", time.Time{}},
				},
			},
		},
		// PUT with PrevValue set ==> CompareAndSwap
		{
			pb.Request{Method: "PUT", ID: 1, PrevValue: "bar"},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "CompareAndSwap",
					Params: []interface{}{"", "bar", uint64(0), "", time.Time{}},
				},
			},
		},
		// PUT with PrevIndex and PrevValue set ==> CompareAndSwap
		{
			pb.Request{Method: "PUT", ID: 1, PrevIndex: 1, PrevValue: "bar"},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "CompareAndSwap",
					Params: []interface{}{"", "bar", uint64(1), "", time.Time{}},
				},
			},
		},
		// DELETE ==> Delete
		{
			pb.Request{Method: "DELETE", ID: 1},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Delete",
					Params: []interface{}{"", false, false},
				},
			},
		},
		// DELETE with PrevIndex set ==> CompareAndDelete
		{
			pb.Request{Method: "DELETE", ID: 1, PrevIndex: 1},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "CompareAndDelete",
					Params: []interface{}{"", "", uint64(1)},
				},
			},
		},
		// DELETE with PrevValue set ==> CompareAndDelete
		{
			pb.Request{Method: "DELETE", ID: 1, PrevValue: "bar"},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "CompareAndDelete",
					Params: []interface{}{"", "bar", uint64(0)},
				},
			},
		},
		// DELETE with PrevIndex *and* PrevValue set ==> CompareAndDelete
		{
			pb.Request{Method: "DELETE", ID: 1, PrevIndex: 5, PrevValue: "bar"},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "CompareAndDelete",
					Params: []interface{}{"", "bar", uint64(5)},
				},
			},
		},
		// QGET ==> Get
		{
			pb.Request{Method: "QGET", ID: 1},
			Response{Event: &store.Event{}},
			[]testutil.Action{
				{
					Name:   "Get",
					Params: []interface{}{"", false, false},
				},
			},
		},
		// SYNC ==> DeleteExpiredKeys
		{
			pb.Request{Method: "SYNC", ID: 1},
			Response{},
			[]testutil.Action{
				{
					Name:   "DeleteExpiredKeys",
					Params: []interface{}{time.Unix(0, 0)},
				},
			},
		},
		{
			pb.Request{Method: "SYNC", ID: 1, Time: 12345},
			Response{},
			[]testutil.Action{
				{
					Name:   "DeleteExpiredKeys",
					Params: []interface{}{time.Unix(0, 12345)},
				},
			},
		},
		// Unknown method - error
		{
			pb.Request{Method: "BADMETHOD", ID: 1},
			Response{err: ErrUnknownMethod},
			[]testutil.Action{},
		},
	}

	for i, tt := range tests {
		st := store.NewRecorder()
		srv := &EtcdServer{store: st}
		resp := srv.applyRequest(tt.req)

		if !reflect.DeepEqual(resp, tt.wresp) {
			t.Errorf("#%d: resp = %+v, want %+v", i, resp, tt.wresp)
		}
		gaction := st.Action()
		if !reflect.DeepEqual(gaction, tt.wactions) {
			t.Errorf("#%d: action = %#v, want %#v", i, gaction, tt.wactions)
		}
	}
}

func TestApplyRequestOnAdminMemberAttributes(t *testing.T) {
	cl := newTestCluster([]*Member{{ID: 1}})
	srv := &EtcdServer{
		store:   store.NewRecorder(),
		cluster: cl,
	}
	req := pb.Request{
		Method: "PUT",
		ID:     1,
		Path:   path.Join(storeMembersPrefix, strconv.FormatUint(1, 16), attributesSuffix),
		Val:    `{"Name":"abc","ClientURLs":["http://127.0.0.1:2379"]}`,
	}
	srv.applyRequest(req)
	w := Attributes{Name: "abc", ClientURLs: []string{"http://127.0.0.1:2379"}}
	if g := cl.Member(1).Attributes; !reflect.DeepEqual(g, w) {
		t.Errorf("attributes = %v, want %v", g, w)
	}
}

func TestApplyConfChangeError(t *testing.T) {
	cl := newCluster("")
	cl.SetStore(store.New())
	for i := 1; i <= 4; i++ {
		cl.AddMember(&Member{ID: types.ID(i)})
	}
	cl.RemoveMember(4)

	tests := []struct {
		cc   raftpb.ConfChange
		werr error
	}{
		{
			raftpb.ConfChange{
				Type:   raftpb.ConfChangeAddNode,
				NodeID: 4,
			},
			ErrIDRemoved,
		},
		{
			raftpb.ConfChange{
				Type:   raftpb.ConfChangeUpdateNode,
				NodeID: 4,
			},
			ErrIDRemoved,
		},
		{
			raftpb.ConfChange{
				Type:   raftpb.ConfChangeAddNode,
				NodeID: 1,
			},
			ErrIDExists,
		},
		{
			raftpb.ConfChange{
				Type:   raftpb.ConfChangeRemoveNode,
				NodeID: 5,
			},
			ErrIDNotFound,
		},
	}
	for i, tt := range tests {
		n := newNodeRecorder()
		srv := &EtcdServer{
			r:       raftNode{Node: n},
			cluster: cl,
			cfg:     &ServerConfig{},
		}
		_, err := srv.applyConfChange(tt.cc, nil)
		if err != tt.werr {
			t.Errorf("#%d: applyConfChange error = %v, want %v", i, err, tt.werr)
		}
		cc := raftpb.ConfChange{Type: tt.cc.Type, NodeID: raft.None}
		w := []testutil.Action{
			{
				Name:   "ApplyConfChange",
				Params: []interface{}{cc},
			},
		}
		if g, _ := n.Wait(1); !reflect.DeepEqual(g, w) {
			t.Errorf("#%d: action = %+v, want %+v", i, g, w)
		}
	}
}

func TestApplyConfChangeShouldStop(t *testing.T) {
	cl := newCluster("")
	cl.SetStore(store.New())
	for i := 1; i <= 3; i++ {
		cl.AddMember(&Member{ID: types.ID(i)})
	}
	srv := &EtcdServer{
		id: 1,
		r: raftNode{
			Node:      newNodeNop(),
			transport: rafthttp.NewNopTransporter(),
		},
		cluster: cl,
	}
	cc := raftpb.ConfChange{
		Type:   raftpb.ConfChangeRemoveNode,
		NodeID: 2,
	}
	// remove non-local member
	shouldStop, err := srv.applyConfChange(cc, &raftpb.ConfState{})
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if shouldStop != false {
		t.Errorf("shouldStop = %t, want %t", shouldStop, false)
	}

	// remove local member
	cc.NodeID = 1
	shouldStop, err = srv.applyConfChange(cc, &raftpb.ConfState{})
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if shouldStop != true {
		t.Errorf("shouldStop = %t, want %t", shouldStop, true)
	}
}

// TestApplyMultiConfChangeShouldStop ensures that apply will return shouldStop
// if the local member is removed along with other conf updates.
func TestApplyMultiConfChangeShouldStop(t *testing.T) {
	cl := newCluster("")
	cl.SetStore(store.New())
	for i := 1; i <= 5; i++ {
		cl.AddMember(&Member{ID: types.ID(i)})
	}
	srv := &EtcdServer{
		id: 2,
		r: raftNode{
			Node:      newNodeNop(),
			transport: rafthttp.NewNopTransporter(),
		},
		cluster: cl,
		w:       wait.New(),
	}
	ents := []raftpb.Entry{}
	for i := 1; i <= 4; i++ {
		ent := raftpb.Entry{
			Term:  1,
			Index: uint64(i),
			Type:  raftpb.EntryConfChange,
			Data: pbutil.MustMarshal(
				&raftpb.ConfChange{
					Type:   raftpb.ConfChangeRemoveNode,
					NodeID: uint64(i)}),
		}
		ents = append(ents, ent)
	}

	_, shouldStop := srv.apply(ents, &raftpb.ConfState{})
	if shouldStop == false {
		t.Errorf("shouldStop = %t, want %t", shouldStop, true)
	}
}

func TestDoProposal(t *testing.T) {
	tests := []pb.Request{
		{Method: "POST", ID: 1},
		{Method: "PUT", ID: 1},
		{Method: "DELETE", ID: 1},
		{Method: "GET", ID: 1, Quorum: true},
	}
	for i, tt := range tests {
		st := store.NewRecorder()
		srv := &EtcdServer{
			cfg: &ServerConfig{TickMs: 1},
			r: raftNode{
				Node:        newNodeCommitter(),
				storage:     &storageRecorder{},
				raftStorage: raft.NewMemoryStorage(),
				transport:   rafthttp.NewNopTransporter(),
			},
			store:    st,
			reqIDGen: idutil.NewGenerator(0, time.Time{}),
		}
		srv.start()
		resp, err := srv.Do(context.Background(), tt)
		srv.Stop()

		action := st.Action()
		if len(action) != 1 {
			t.Errorf("#%d: len(action) = %d, want 1", i, len(action))
		}
		if err != nil {
			t.Fatalf("#%d: err = %v, want nil", i, err)
		}
		wresp := Response{Event: &store.Event{}}
		if !reflect.DeepEqual(resp, wresp) {
			t.Errorf("#%d: resp = %v, want %v", i, resp, wresp)
		}
	}
}

func TestDoProposalCancelled(t *testing.T) {
	wait := wait.NewRecorder()
	srv := &EtcdServer{
		cfg:      &ServerConfig{TickMs: 1},
		r:        raftNode{Node: newNodeNop()},
		w:        wait,
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := srv.Do(ctx, pb.Request{Method: "PUT"})

	if err != ErrCanceled {
		t.Fatalf("err = %v, want %v", err, ErrCanceled)
	}
	w := []testutil.Action{{Name: "Register"}, {Name: "Trigger"}}
	if !reflect.DeepEqual(wait.Action(), w) {
		t.Errorf("wait.action = %+v, want %+v", wait.Action(), w)
	}
}

func TestDoProposalTimeout(t *testing.T) {
	srv := &EtcdServer{
		cfg:      &ServerConfig{TickMs: 1},
		r:        raftNode{Node: newNodeNop()},
		w:        wait.NewNop(),
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	ctx, _ := context.WithTimeout(context.Background(), 0)
	_, err := srv.Do(ctx, pb.Request{Method: "PUT"})
	if err != ErrTimeout {
		t.Fatalf("err = %v, want %v", err, ErrTimeout)
	}
}

func TestDoProposalStopped(t *testing.T) {
	srv := &EtcdServer{
		cfg:      &ServerConfig{TickMs: 1},
		r:        raftNode{Node: newNodeNop()},
		w:        wait.NewNop(),
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	srv.done = make(chan struct{})
	close(srv.done)
	_, err := srv.Do(context.Background(), pb.Request{Method: "PUT", ID: 1})
	if err != ErrStopped {
		t.Errorf("err = %v, want %v", err, ErrStopped)
	}
}

// TestSync tests sync 1. is nonblocking 2. proposes SYNC request.
func TestSync(t *testing.T) {
	n := newNodeRecorder()
	srv := &EtcdServer{
		r:        raftNode{Node: n},
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	// check that sync is non-blocking
	done := make(chan struct{})
	go func() {
		srv.sync(10 * time.Second)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sync should be non-blocking but did not return after 1s!")
	}

	action, _ := n.Wait(1)
	if len(action) != 1 {
		t.Fatalf("len(action) = %d, want 1", len(action))
	}
	if action[0].Name != "Propose" {
		t.Fatalf("action = %s, want Propose", action[0].Name)
	}
	data := action[0].Params[0].([]byte)
	var r pb.Request
	if err := r.Unmarshal(data); err != nil {
		t.Fatalf("unmarshal request error: %v", err)
	}
	if r.Method != "SYNC" {
		t.Errorf("method = %s, want SYNC", r.Method)
	}
}

// TestSyncTimeout tests the case that sync 1. is non-blocking 2. cancel request
// after timeout
func TestSyncTimeout(t *testing.T) {
	n := newProposalBlockerRecorder()
	srv := &EtcdServer{
		r:        raftNode{Node: n},
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	// check that sync is non-blocking
	done := make(chan struct{})
	go func() {
		srv.sync(0)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sync should be non-blocking but did not return after 1s!")
	}

	w := []testutil.Action{{Name: "Propose blocked"}}
	if g, _ := n.Wait(1); !reflect.DeepEqual(g, w) {
		t.Errorf("action = %v, want %v", g, w)
	}
}

// TODO: TestNoSyncWhenNoLeader

// TestSyncTrigger tests that the server proposes a SYNC request when its sync timer ticks
func TestSyncTrigger(t *testing.T) {
	n := newReadyNode()
	st := make(chan time.Time, 1)
	srv := &EtcdServer{
		cfg: &ServerConfig{TickMs: 1},
		r: raftNode{
			Node:        n,
			raftStorage: raft.NewMemoryStorage(),
			transport:   rafthttp.NewNopTransporter(),
			storage:     &storageRecorder{},
		},
		store:      store.NewNop(),
		SyncTicker: st,
		reqIDGen:   idutil.NewGenerator(0, time.Time{}),
	}

	// trigger the server to become a leader and accept sync requests
	go func() {
		srv.start()
		n.readyc <- raft.Ready{
			SoftState: &raft.SoftState{
				RaftState: raft.StateLeader,
			},
		}
		// trigger a sync request
		st <- time.Time{}
	}()

	action, _ := n.Wait(1)
	go srv.Stop()

	if len(action) != 1 {
		t.Fatalf("len(action) = %d, want 1", len(action))
	}
	if action[0].Name != "Propose" {
		t.Fatalf("action = %s, want Propose", action[0].Name)
	}
	data := action[0].Params[0].([]byte)
	var req pb.Request
	if err := req.Unmarshal(data); err != nil {
		t.Fatalf("error unmarshalling data: %v", err)
	}
	if req.Method != "SYNC" {
		t.Fatalf("unexpected proposed request: %#v", req.Method)
	}

	// wait on stop message
	<-n.Chan()
}

// snapshot should snapshot the store and cut the persistent
func TestSnapshot(t *testing.T) {
	s := raft.NewMemoryStorage()
	s.Append([]raftpb.Entry{{Index: 1}})
	st := store.NewRecorder()
	p := &storageRecorder{}
	srv := &EtcdServer{
		cfg: &ServerConfig{},
		r: raftNode{
			Node:        newNodeNop(),
			raftStorage: s,
			storage:     p,
		},
		store: st,
	}
	srv.snapshot(1, raftpb.ConfState{Nodes: []uint64{1}})
	gaction, _ := st.Wait(2)
	if len(gaction) != 2 {
		t.Fatalf("len(action) = %d, want 1", len(gaction))
	}
	if !reflect.DeepEqual(gaction[0], testutil.Action{Name: "Clone"}) {
		t.Errorf("action = %s, want Clone", gaction[0])
	}
	if !reflect.DeepEqual(gaction[1], testutil.Action{Name: "SaveNoCopy"}) {
		t.Errorf("action = %s, want SaveNoCopy", gaction[1])
	}
	gaction = p.Action()
	if len(gaction) != 1 {
		t.Fatalf("len(action) = %d, want 1", len(gaction))
	}
	if !reflect.DeepEqual(gaction[0], testutil.Action{Name: "SaveSnap"}) {
		t.Errorf("action = %s, want SaveSnap", gaction[0])
	}
}

// Applied > SnapCount should trigger a SaveSnap event
func TestTriggerSnap(t *testing.T) {
	snapc := 10
	st := store.NewRecorder()
	p := &storageRecorder{}
	srv := &EtcdServer{
		cfg:       &ServerConfig{TickMs: 1},
		snapCount: uint64(snapc),
		r: raftNode{
			Node:        newNodeCommitter(),
			raftStorage: raft.NewMemoryStorage(),
			storage:     p,
			transport:   rafthttp.NewNopTransporter(),
		},
		store:    st,
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	srv.start()
	for i := 0; i < snapc+1; i++ {
		srv.Do(context.Background(), pb.Request{Method: "PUT"})
	}

	wcnt := 2 + snapc
	gaction, _ := p.Wait(wcnt)

	srv.Stop()

	// each operation is recorded as a Save
	// (SnapCount+1) * Puts + SaveSnap = (SnapCount+1) * Save + SaveSnap
	if len(gaction) != wcnt {
		t.Fatalf("len(action) = %d, want %d", len(gaction), wcnt)
	}
	if !reflect.DeepEqual(gaction[wcnt-1], testutil.Action{Name: "SaveSnap"}) {
		t.Errorf("action = %s, want SaveSnap", gaction[wcnt-1])
	}
}

// TestConcurrentApplyAndSnapshotV3 will send out snapshots concurrently with
// proposals.
func TestConcurrentApplyAndSnapshotV3(t *testing.T) {
	const (
		// snapshots that may queue up at once without dropping
		maxInFlightMsgSnap = 16
	)
	n := newNopReadyNode()
	cl := newCluster("abc")
	cl.SetStore(store.New())

	testdir, err := ioutil.TempDir(os.TempDir(), "testsnapdir")
	if err != nil {
		t.Fatalf("Couldn't open tempdir (%v)", err)
	}
	defer os.RemoveAll(testdir)
	if err := os.MkdirAll(testdir+"/member/snap", 0755); err != nil {
		t.Fatalf("Couldn't make snap dir (%v)", err)
	}

	rs := raft.NewMemoryStorage()
	tr, snapDoneC := rafthttp.NewSnapTransporter(testdir)
	s := &EtcdServer{
		cfg: &ServerConfig{
			V3demo:  true,
			DataDir: testdir,
		},
		r: raftNode{
			Node:        n,
			transport:   tr,
			storage:     &storageRecorder{dbPath: testdir},
			raftStorage: rs,
		},
		store:    cl.store,
		cluster:  cl,
		msgSnapC: make(chan raftpb.Message, maxInFlightMsgSnap),
	}

	be, tmpPath := backend.NewDefaultTmpBackend()
	defer func() {
		os.RemoveAll(tmpPath)
	}()
	s.kv = dstorage.New(be, &lease.FakeLessor{}, &s.consistIndex)
	s.be = be

	s.start()
	defer s.Stop()

	// submit applied entries and snap entries
	idx := uint64(0)
	outdated := 0
	accepted := 0
	for k := 1; k <= 101; k++ {
		idx++
		ch := s.w.Register(uint64(idx))
		req := &pb.Request{Method: "QGET", ID: uint64(idx)}
		ent := raftpb.Entry{Index: uint64(idx), Data: pbutil.MustMarshal(req)}
		ready := raft.Ready{Entries: []raftpb.Entry{ent}}
		n.readyc <- ready

		ready = raft.Ready{CommittedEntries: []raftpb.Entry{ent}}
		n.readyc <- ready

		// "idx" applied
		<-ch

		// one snapshot for every two messages
		if k%2 != 0 {
			continue
		}

		n.readyc <- raft.Ready{Messages: []raftpb.Message{{Type: raftpb.MsgSnap}}}
		// get the snapshot sent by the transport
		snapMsg := <-snapDoneC
		// If the snapshot trails applied records, recovery will panic
		// since there's no allocated snapshot at the place of the
		// snapshot record. This only happens when the applier and the
		// snapshot sender get out of sync.
		if snapMsg.Snapshot.Metadata.Index == idx {
			idx++
			snapMsg.Snapshot.Metadata.Index = idx
			ready = raft.Ready{Snapshot: snapMsg.Snapshot}
			n.readyc <- ready
			accepted++
		} else {
			outdated++
		}
		// don't wait for the snapshot to complete, move to next message
	}
	if accepted != 50 {
		t.Errorf("accepted=%v, want 50", accepted)
	}
	if outdated != 0 {
		t.Errorf("outdated=%v, want 0", outdated)
	}
}

// TestRecvSnapshot tests when it receives a snapshot from raft leader,
// it should trigger storage.SaveSnap and also store.Recover.
func TestRecvSnapshot(t *testing.T) {
	n := newNopReadyNode()
	st := store.NewRecorder()
	p := &storageRecorder{}
	cl := newCluster("abc")
	cl.SetStore(store.New())
	s := &EtcdServer{
		cfg: &ServerConfig{},
		r: raftNode{
			Node:        n,
			transport:   rafthttp.NewNopTransporter(),
			storage:     p,
			raftStorage: raft.NewMemoryStorage(),
		},
		store:   st,
		cluster: cl,
	}

	s.start()
	n.readyc <- raft.Ready{Snapshot: raftpb.Snapshot{Metadata: raftpb.SnapshotMetadata{Index: 1}}}

	// wait for actions happened on the storage
	for len(p.Action()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	s.Stop()

	wactions := []testutil.Action{{Name: "Recovery"}}
	if g := st.Action(); !reflect.DeepEqual(g, wactions) {
		t.Errorf("store action = %v, want %v", g, wactions)
	}
	wactions = []testutil.Action{{Name: "SaveSnap"}, {Name: "Save"}}
	if g := p.Action(); !reflect.DeepEqual(g, wactions) {
		t.Errorf("storage action = %v, want %v", g, wactions)
	}
}

// TestApplySnapshotAndCommittedEntries tests that server applies snapshot
// first and then committed entries.
func TestApplySnapshotAndCommittedEntries(t *testing.T) {
	n := newNopReadyNode()
	st := store.NewRecorderStream()
	cl := newCluster("abc")
	cl.SetStore(store.New())
	storage := raft.NewMemoryStorage()
	s := &EtcdServer{
		cfg: &ServerConfig{},
		r: raftNode{
			Node:        n,
			storage:     &storageRecorder{},
			raftStorage: storage,
			transport:   rafthttp.NewNopTransporter(),
		},
		store:   st,
		cluster: cl,
	}

	s.start()
	req := &pb.Request{Method: "QGET"}
	n.readyc <- raft.Ready{
		Snapshot: raftpb.Snapshot{Metadata: raftpb.SnapshotMetadata{Index: 1}},
		CommittedEntries: []raftpb.Entry{
			{Index: 2, Data: pbutil.MustMarshal(req)},
		},
	}
	// make goroutines move forward to receive snapshot
	actions, _ := st.Wait(2)
	s.Stop()

	if len(actions) != 2 {
		t.Fatalf("len(action) = %d, want 2", len(actions))
	}
	if actions[0].Name != "Recovery" {
		t.Errorf("actions[0] = %s, want %s", actions[0].Name, "Recovery")
	}
	if actions[1].Name != "Get" {
		t.Errorf("actions[1] = %s, want %s", actions[1].Name, "Get")
	}
}

// TestAddMember tests AddMember can propose and perform node addition.
func TestAddMember(t *testing.T) {
	n := newNodeConfChangeCommitterRecorder()
	n.readyc <- raft.Ready{
		SoftState: &raft.SoftState{RaftState: raft.StateLeader},
	}
	cl := newTestCluster(nil)
	st := store.New()
	cl.SetStore(st)
	s := &EtcdServer{
		r: raftNode{
			Node:        n,
			raftStorage: raft.NewMemoryStorage(),
			storage:     &storageRecorder{},
			transport:   rafthttp.NewNopTransporter(),
		},
		cfg:      &ServerConfig{},
		store:    st,
		cluster:  cl,
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	s.start()
	m := Member{ID: 1234, RaftAttributes: RaftAttributes{PeerURLs: []string{"foo"}}}
	err := s.AddMember(context.TODO(), m)
	gaction := n.Action()
	s.Stop()

	if err != nil {
		t.Fatalf("AddMember error: %v", err)
	}
	wactions := []testutil.Action{{Name: "ProposeConfChange:ConfChangeAddNode"}, {Name: "ApplyConfChange:ConfChangeAddNode"}}
	if !reflect.DeepEqual(gaction, wactions) {
		t.Errorf("action = %v, want %v", gaction, wactions)
	}
	if cl.Member(1234) == nil {
		t.Errorf("member with id 1234 is not added")
	}
}

// TestRemoveMember tests RemoveMember can propose and perform node removal.
func TestRemoveMember(t *testing.T) {
	n := newNodeConfChangeCommitterRecorder()
	n.readyc <- raft.Ready{
		SoftState: &raft.SoftState{RaftState: raft.StateLeader},
	}
	cl := newTestCluster(nil)
	st := store.New()
	cl.SetStore(store.New())
	cl.AddMember(&Member{ID: 1234})
	s := &EtcdServer{
		r: raftNode{
			Node:        n,
			raftStorage: raft.NewMemoryStorage(),
			storage:     &storageRecorder{},
			transport:   rafthttp.NewNopTransporter(),
		},
		cfg:      &ServerConfig{},
		store:    st,
		cluster:  cl,
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	s.start()
	err := s.RemoveMember(context.TODO(), 1234)
	gaction := n.Action()
	s.Stop()

	if err != nil {
		t.Fatalf("RemoveMember error: %v", err)
	}
	wactions := []testutil.Action{{Name: "ProposeConfChange:ConfChangeRemoveNode"}, {Name: "ApplyConfChange:ConfChangeRemoveNode"}}
	if !reflect.DeepEqual(gaction, wactions) {
		t.Errorf("action = %v, want %v", gaction, wactions)
	}
	if cl.Member(1234) != nil {
		t.Errorf("member with id 1234 is not removed")
	}
}

// TestUpdateMember tests RemoveMember can propose and perform node update.
func TestUpdateMember(t *testing.T) {
	n := newNodeConfChangeCommitterRecorder()
	n.readyc <- raft.Ready{
		SoftState: &raft.SoftState{RaftState: raft.StateLeader},
	}
	cl := newTestCluster(nil)
	st := store.New()
	cl.SetStore(st)
	cl.AddMember(&Member{ID: 1234})
	s := &EtcdServer{
		r: raftNode{
			Node:        n,
			raftStorage: raft.NewMemoryStorage(),
			storage:     &storageRecorder{},
			transport:   rafthttp.NewNopTransporter(),
		},
		store:    st,
		cluster:  cl,
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	s.start()
	wm := Member{ID: 1234, RaftAttributes: RaftAttributes{PeerURLs: []string{"http://127.0.0.1:1"}}}
	err := s.UpdateMember(context.TODO(), wm)
	gaction := n.Action()
	s.Stop()

	if err != nil {
		t.Fatalf("UpdateMember error: %v", err)
	}
	wactions := []testutil.Action{{Name: "ProposeConfChange:ConfChangeUpdateNode"}, {Name: "ApplyConfChange:ConfChangeUpdateNode"}}
	if !reflect.DeepEqual(gaction, wactions) {
		t.Errorf("action = %v, want %v", gaction, wactions)
	}
	if !reflect.DeepEqual(cl.Member(1234), &wm) {
		t.Errorf("member = %v, want %v", cl.Member(1234), &wm)
	}
}

// TODO: test server could stop itself when being removed

func TestPublish(t *testing.T) {
	n := newNodeRecorder()
	ch := make(chan interface{}, 1)
	// simulate that request has gone through consensus
	ch <- Response{}
	w := wait.NewWithResponse(ch)
	srv := &EtcdServer{
		cfg:        &ServerConfig{TickMs: 1},
		id:         1,
		r:          raftNode{Node: n},
		attributes: Attributes{Name: "node1", ClientURLs: []string{"http://a", "http://b"}},
		cluster:    &cluster{},
		w:          w,
		reqIDGen:   idutil.NewGenerator(0, time.Time{}),
	}
	srv.publish(time.Hour)

	action := n.Action()
	if len(action) != 1 {
		t.Fatalf("len(action) = %d, want 1", len(action))
	}
	if action[0].Name != "Propose" {
		t.Fatalf("action = %s, want Propose", action[0].Name)
	}
	data := action[0].Params[0].([]byte)
	var r pb.Request
	if err := r.Unmarshal(data); err != nil {
		t.Fatalf("unmarshal request error: %v", err)
	}
	if r.Method != "PUT" {
		t.Errorf("method = %s, want PUT", r.Method)
	}
	wm := Member{ID: 1, Attributes: Attributes{Name: "node1", ClientURLs: []string{"http://a", "http://b"}}}
	if wpath := path.Join(memberStoreKey(wm.ID), attributesSuffix); r.Path != wpath {
		t.Errorf("path = %s, want %s", r.Path, wpath)
	}
	var gattr Attributes
	if err := json.Unmarshal([]byte(r.Val), &gattr); err != nil {
		t.Fatalf("unmarshal val error: %v", err)
	}
	if !reflect.DeepEqual(gattr, wm.Attributes) {
		t.Errorf("member = %v, want %v", gattr, wm.Attributes)
	}
}

// TestPublishStopped tests that publish will be stopped if server is stopped.
func TestPublishStopped(t *testing.T) {
	srv := &EtcdServer{
		cfg: &ServerConfig{TickMs: 1},
		r: raftNode{
			Node:      newNodeNop(),
			transport: rafthttp.NewNopTransporter(),
		},
		cluster:  &cluster{},
		w:        wait.NewNop(),
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	close(srv.done)
	srv.publish(time.Hour)
}

// TestPublishRetry tests that publish will keep retry until success.
func TestPublishRetry(t *testing.T) {
	n := newNodeRecorder()
	srv := &EtcdServer{
		cfg:      &ServerConfig{TickMs: 1},
		r:        raftNode{Node: n},
		w:        wait.NewNop(),
		done:     make(chan struct{}),
		reqIDGen: idutil.NewGenerator(0, time.Time{}),
	}
	// TODO: use fakeClockwork
	time.AfterFunc(10*time.Millisecond, func() { close(srv.done) })
	srv.publish(10 * time.Nanosecond)

	action := n.Action()
	// multiple Proposes
	if cnt := len(action); cnt < 2 {
		t.Errorf("len(action) = %d, want >= 2", cnt)
	}
}

func TestUpdateVersion(t *testing.T) {
	n := newNodeRecorder()
	ch := make(chan interface{}, 1)
	// simulate that request has gone through consensus
	ch <- Response{}
	w := wait.NewWithResponse(ch)
	srv := &EtcdServer{
		id:         1,
		cfg:        &ServerConfig{TickMs: 1},
		r:          raftNode{Node: n},
		attributes: Attributes{Name: "node1", ClientURLs: []string{"http://node1.com"}},
		cluster:    &cluster{},
		w:          w,
		reqIDGen:   idutil.NewGenerator(0, time.Time{}),
	}
	srv.updateClusterVersion("2.0.0")

	action := n.Action()
	if len(action) != 1 {
		t.Fatalf("len(action) = %d, want 1", len(action))
	}
	if action[0].Name != "Propose" {
		t.Fatalf("action = %s, want Propose", action[0].Name)
	}
	data := action[0].Params[0].([]byte)
	var r pb.Request
	if err := r.Unmarshal(data); err != nil {
		t.Fatalf("unmarshal request error: %v", err)
	}
	if r.Method != "PUT" {
		t.Errorf("method = %s, want PUT", r.Method)
	}
	if wpath := path.Join(StoreClusterPrefix, "version"); r.Path != wpath {
		t.Errorf("path = %s, want %s", r.Path, wpath)
	}
	if r.Val != "2.0.0" {
		t.Errorf("val = %s, want %s", r.Val, "2.0.0")
	}
}

func TestStopNotify(t *testing.T) {
	s := &EtcdServer{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go func() {
		<-s.stop
		close(s.done)
	}()

	notifier := s.StopNotify()
	select {
	case <-notifier:
		t.Fatalf("received unexpected stop notification")
	default:
	}
	s.Stop()
	select {
	case <-notifier:
	default:
		t.Fatalf("cannot receive stop notification")
	}
}

func TestGetOtherPeerURLs(t *testing.T) {
	tests := []struct {
		membs []*Member
		self  string
		wurls []string
	}{
		{
			[]*Member{
				newTestMember(1, []string{"http://10.0.0.1"}, "a", nil),
			},
			"a",
			[]string{},
		},
		{
			[]*Member{
				newTestMember(1, []string{"http://10.0.0.1"}, "a", nil),
				newTestMember(2, []string{"http://10.0.0.2"}, "b", nil),
				newTestMember(3, []string{"http://10.0.0.3"}, "c", nil),
			},
			"a",
			[]string{"http://10.0.0.2", "http://10.0.0.3"},
		},
		{
			[]*Member{
				newTestMember(1, []string{"http://10.0.0.1"}, "a", nil),
				newTestMember(3, []string{"http://10.0.0.3"}, "c", nil),
				newTestMember(2, []string{"http://10.0.0.2"}, "b", nil),
			},
			"a",
			[]string{"http://10.0.0.2", "http://10.0.0.3"},
		},
	}
	for i, tt := range tests {
		cl := newClusterFromMembers("", types.ID(0), tt.membs)
		urls := getRemotePeerURLs(cl, tt.self)
		if !reflect.DeepEqual(urls, tt.wurls) {
			t.Errorf("#%d: urls = %+v, want %+v", i, urls, tt.wurls)
		}
	}
}

type nodeRecorder struct{ testutil.Recorder }

func newNodeRecorder() *nodeRecorder { return &nodeRecorder{&testutil.RecorderBuffered{}} }
func newNodeNop() raft.Node          { return newNodeRecorder() }

func (n *nodeRecorder) Tick() { n.Record(testutil.Action{Name: "Tick"}) }
func (n *nodeRecorder) Campaign(ctx context.Context) error {
	n.Record(testutil.Action{Name: "Campaign"})
	return nil
}
func (n *nodeRecorder) Propose(ctx context.Context, data []byte) error {
	n.Record(testutil.Action{Name: "Propose", Params: []interface{}{data}})
	return nil
}
func (n *nodeRecorder) ProposeConfChange(ctx context.Context, conf raftpb.ConfChange) error {
	n.Record(testutil.Action{Name: "ProposeConfChange"})
	return nil
}
func (n *nodeRecorder) Step(ctx context.Context, msg raftpb.Message) error {
	n.Record(testutil.Action{Name: "Step"})
	return nil
}
func (n *nodeRecorder) Status() raft.Status      { return raft.Status{} }
func (n *nodeRecorder) Ready() <-chan raft.Ready { return nil }
func (n *nodeRecorder) Advance()                 {}
func (n *nodeRecorder) ApplyConfChange(conf raftpb.ConfChange) *raftpb.ConfState {
	n.Record(testutil.Action{Name: "ApplyConfChange", Params: []interface{}{conf}})
	return &raftpb.ConfState{}
}

func (n *nodeRecorder) Stop() {
	n.Record(testutil.Action{Name: "Stop"})
}

func (n *nodeRecorder) ReportUnreachable(id uint64) {}

func (n *nodeRecorder) ReportSnapshot(id uint64, status raft.SnapshotStatus) {}

func (n *nodeRecorder) Compact(index uint64, nodes []uint64, d []byte) {
	n.Record(testutil.Action{Name: "Compact"})
}

type nodeProposalBlockerRecorder struct {
	nodeRecorder
}

func newProposalBlockerRecorder() *nodeProposalBlockerRecorder {
	return &nodeProposalBlockerRecorder{*newNodeRecorder()}
}

func (n *nodeProposalBlockerRecorder) Propose(ctx context.Context, data []byte) error {
	<-ctx.Done()
	n.Record(testutil.Action{Name: "Propose blocked"})
	return nil
}

// readyNode is a nodeRecorder with a user-writeable ready channel
type readyNode struct {
	nodeRecorder
	readyc chan raft.Ready
}

func newReadyNode() *readyNode {
	return &readyNode{
		nodeRecorder{testutil.NewRecorderStream()},
		make(chan raft.Ready, 1)}
}
func newNopReadyNode() *readyNode {
	return &readyNode{*newNodeRecorder(), make(chan raft.Ready, 1)}
}

func (n *readyNode) Ready() <-chan raft.Ready { return n.readyc }

type nodeConfChangeCommitterRecorder struct {
	readyNode
	index uint64
}

func newNodeConfChangeCommitterRecorder() *nodeConfChangeCommitterRecorder {
	return &nodeConfChangeCommitterRecorder{*newNopReadyNode(), 0}
}

func (n *nodeConfChangeCommitterRecorder) ProposeConfChange(ctx context.Context, conf raftpb.ConfChange) error {
	data, err := conf.Marshal()
	if err != nil {
		return err
	}
	n.index++
	n.Record(testutil.Action{Name: "ProposeConfChange:" + conf.Type.String()})
	n.readyc <- raft.Ready{CommittedEntries: []raftpb.Entry{{Index: n.index, Type: raftpb.EntryConfChange, Data: data}}}
	return nil
}
func (n *nodeConfChangeCommitterRecorder) Ready() <-chan raft.Ready {
	return n.readyc
}
func (n *nodeConfChangeCommitterRecorder) ApplyConfChange(conf raftpb.ConfChange) *raftpb.ConfState {
	n.Record(testutil.Action{Name: "ApplyConfChange:" + conf.Type.String()})
	return &raftpb.ConfState{}
}

// nodeCommitter commits proposed data immediately.
type nodeCommitter struct {
	readyNode
	index uint64
}

func newNodeCommitter() raft.Node {
	return &nodeCommitter{*newNopReadyNode(), 0}
}
func (n *nodeCommitter) Propose(ctx context.Context, data []byte) error {
	n.index++
	ents := []raftpb.Entry{{Index: n.index, Data: data}}
	n.readyc <- raft.Ready{
		Entries:          ents,
		CommittedEntries: ents,
	}
	return nil
}
