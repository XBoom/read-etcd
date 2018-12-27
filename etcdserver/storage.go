// Copyright 2015 The etcd Authors
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
	"io"

	"go.etcd.io/etcd/v3/etcdserver/api/snap"
	pb "go.etcd.io/etcd/v3/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/v3/pkg/pbutil"
	"go.etcd.io/etcd/v3/pkg/types"
	"go.etcd.io/etcd/v3/raft/raftpb"
	"go.etcd.io/etcd/v3/wal"
	"go.etcd.io/etcd/v3/wal/walpb"

	"go.uber.org/zap"
)

type Storage interface {
	// Save function saves ents and state to the underlying stable storage.
	// Save MUST block until st and ents are on stable storage.
	Save(st raftpb.HardState, ents []raftpb.Entry) error
	// SaveSnap function saves snapshot to the underlying stable storage.
	SaveSnap(snap raftpb.Snapshot) error
	// Close closes the Storage and performs finalization.
	Close() error

	// SaveSnapshot function saves only snapshot to the underlying stable storage.
	SaveSnapshot(snap raftpb.Snapshot) error
	// SaveAll function saves ents, snapshot and state to the underlying stable storage.
	// SaveAll MUST block until st and ents are on stable storage.
	SaveAll(st raftpb.HardState, ents []raftpb.Entry, snap raftpb.Snapshot) error
	// Release release release the locked wal files since they will not be used.
	Release(snap raftpb.Snapshot) error
}

type storage struct {
	*wal.WAL
	*snap.Snapshotter
}

func NewStorage(w *wal.WAL, s *snap.Snapshotter) Storage {
	return &storage{w, s}
}

// SaveSnap saves the snapshot to disk and release the locked
// wal files since they will not be used.
func (st *storage) SaveSnap(snap raftpb.Snapshot) error {
	walsnap := walpb.Snapshot{
		Index: snap.Metadata.Index,
		Term:  snap.Metadata.Term,
	}
	err := st.WAL.SaveSnapshot(walsnap)
	if err != nil {
		return err
	}
	err = st.Snapshotter.SaveSnap(snap)
	if err != nil {
		return err
	}
	return st.WAL.ReleaseLockTo(snap.Metadata.Index)
}

// SaveSnapshot saves the snapshot to disk.
func (st *storage) SaveSnapshot(snap raftpb.Snapshot) error {
	return st.Snapshotter.SaveSnap(snap)
}

func (st *storage) Release(snap raftpb.Snapshot) error {
	return st.WAL.ReleaseLockTo(snap.Metadata.Index)
}

func checkWALSnap(lg *zap.Logger, waldir string, snapshot *raftpb.Snapshot) bool {
	if snapshot == nil {
		lg.Fatal("checkWALSnap: snapshot is empty")
	}

	walsnap := walpb.Snapshot{
		Index: snapshot.Metadata.Index,
		Term:  snapshot.Metadata.Term,
	}

	w, _, _, st, _ := readWAL(lg, waldir, walsnap)
	defer w.Close()

	lg.Info(
		"checkWALSnap: snapshot and hardstate data",
		zap.Uint64("snapshot-index", snapshot.Metadata.Index),
		zap.Uint64("st-commit", st.Commit),
	)

	if snapshot.Metadata.Index > st.Commit {
		return false
	}

	return true
}

func readWAL(lg *zap.Logger, waldir string, snap walpb.Snapshot) (w *wal.WAL, id, cid types.ID, st raftpb.HardState, ents []raftpb.Entry) {
	var (
		err       error
		wmetadata []byte
	)

	repaired := false
	for {
		if w, err = wal.Open(lg, waldir, snap); err != nil {
			lg.Fatal("failed to open WAL", zap.Error(err))
		}
		if wmetadata, st, ents, err = w.ReadAll(); err != nil {
			w.Close()
			// we can only repair ErrUnexpectedEOF and we never repair twice.
			if repaired || err != io.ErrUnexpectedEOF {
				lg.Fatal("failed to read WAL, cannot be repaired", zap.Error(err))
			}
			if !wal.Repair(lg, waldir) {
				lg.Fatal("failed to repair WAL", zap.Error(err))
			} else {
				lg.Info("repaired WAL", zap.Error(err))
				repaired = true
			}
			continue
		}
		break
	}
	var metadata pb.Metadata
	pbutil.MustUnmarshal(&metadata, wmetadata)
	id = types.ID(metadata.NodeID)
	cid = types.ID(metadata.ClusterID)
	return w, id, cid, st, ents
}
