// Copyright 2016 The Noms Authors. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

package chunks

import (
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/attic-labs/noms/d"
	"github.com/attic-labs/noms/hash"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

const (
	rootKeyConst     = "/root"
	chunkPrefixConst = "/chunk/"
)

type LevelDBStoreFlags struct {
	maxFileHandles int
	dumpStats      bool
}

var (
	ldbFlags        = LevelDBStoreFlags{24, false}
	flagsRegistered = false
)

func RegisterLevelDBFlags() {
	if !flagsRegistered {
		flagsRegistered = true
		flag.IntVar(&ldbFlags.maxFileHandles, "ldb-max-file-handles", 24, "max number of open file handles")
		flag.BoolVar(&ldbFlags.dumpStats, "ldb-dump-stats", false, "print get/has/put counts on close")
	}
}

func NewLevelDBStoreUseFlags(dir, ns string) *LevelDBStore {
	return newLevelDBStore(newBackingStore(dir, ldbFlags.maxFileHandles, ldbFlags.dumpStats), []byte(ns), true)
}

func NewLevelDBStore(dir, ns string, maxFileHandles int, dumpStats bool) *LevelDBStore {
	return newLevelDBStore(newBackingStore(dir, maxFileHandles, dumpStats), []byte(ns), true)
}

func newLevelDBStore(store *internalLevelDBStore, ns []byte, closeBackingStore bool) *LevelDBStore {
	return &LevelDBStore{
		internalLevelDBStore: store,
		rootKey:              append(ns, []byte(rootKeyConst)...),
		chunkPrefix:          append(ns, []byte(chunkPrefixConst)...),
		closeBackingStore:    closeBackingStore,
	}
}

type LevelDBStore struct {
	*internalLevelDBStore
	rootKey           []byte
	chunkPrefix       []byte
	closeBackingStore bool
}

func (l *LevelDBStore) Root() hash.Hash {
	d.Chk.NotNil(l.internalLevelDBStore, "Cannot use LevelDBStore after Close().")
	return l.rootByKey(l.rootKey)
}

func (l *LevelDBStore) UpdateRoot(current, last hash.Hash) bool {
	d.Chk.NotNil(l.internalLevelDBStore, "Cannot use LevelDBStore after Close().")
	return l.updateRootByKey(l.rootKey, current, last)
}

func (l *LevelDBStore) Get(ref hash.Hash) Chunk {
	d.Chk.NotNil(l.internalLevelDBStore, "Cannot use LevelDBStore after Close().")
	return l.getByKey(l.toChunkKey(ref), ref)
}

func (l *LevelDBStore) Has(ref hash.Hash) bool {
	d.Chk.NotNil(l.internalLevelDBStore, "Cannot use LevelDBStore after Close().")
	return l.hasByKey(l.toChunkKey(ref))
}

func (l *LevelDBStore) Put(c Chunk) {
	d.Chk.NotNil(l.internalLevelDBStore, "Cannot use LevelDBStore after Close().")
	l.putByKey(l.toChunkKey(c.Hash()), c)
}

func (l *LevelDBStore) PutMany(chunks []Chunk) (e BackpressureError) {
	numBytes := 0
	b := new(leveldb.Batch)
	for _, c := range chunks {
		d := c.Data()
		numBytes += len(d)
		b.Put(l.toChunkKey(c.Hash()), d)
	}
	l.putBatch(b, numBytes)
	return
}

func (l *LevelDBStore) Close() error {
	if l.closeBackingStore {
		l.internalLevelDBStore.Close()
	}
	l.internalLevelDBStore = nil
	return nil
}

func (l *LevelDBStore) toChunkKey(r hash.Hash) []byte {
	digest := r.DigestSlice()
	out := make([]byte, len(l.chunkPrefix), len(l.chunkPrefix)+len(digest))
	copy(out, l.chunkPrefix)
	return append(out, digest...)
}

type internalLevelDBStore struct {
	db                                     *leveldb.DB
	mu                                     *sync.Mutex
	concurrentWriteLimit                   chan struct{}
	getCount, hasCount, putCount, putBytes int64
	dumpStats                              bool
}

func newBackingStore(dir string, maxFileHandles int, dumpStats bool) *internalLevelDBStore {
	d.Exp.NotEmpty(dir)
	d.Exp.NoError(os.MkdirAll(dir, 0700))
	db, err := leveldb.OpenFile(dir, &opt.Options{
		Compression:            opt.SnappyCompression,
		Filter:                 filter.NewBloomFilter(10), // 10 bits/key
		OpenFilesCacheCapacity: maxFileHandles,
		WriteBuffer:            1 << 24, // 16MiB,
	})
	d.Chk.NoError(err, "opening internalLevelDBStore in %s", dir)
	return &internalLevelDBStore{
		db:                   db,
		mu:                   &sync.Mutex{},
		concurrentWriteLimit: make(chan struct{}, maxFileHandles),
		dumpStats:            dumpStats,
	}
}

func (l *internalLevelDBStore) rootByKey(key []byte) hash.Hash {
	val, err := l.db.Get(key, nil)
	if err == errors.ErrNotFound {
		return hash.Hash{}
	}
	d.Chk.NoError(err)

	return hash.Parse(string(val))
}

func (l *internalLevelDBStore) updateRootByKey(key []byte, current, last hash.Hash) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last != l.rootByKey(key) {
		return false
	}

	// Sync: true write option should fsync memtable data to disk
	err := l.db.Put(key, []byte(current.String()), &opt.WriteOptions{Sync: true})
	d.Chk.NoError(err)
	return true
}

func (l *internalLevelDBStore) getByKey(key []byte, ref hash.Hash) Chunk {
	data, err := l.db.Get(key, nil)
	l.getCount++
	if err == errors.ErrNotFound {
		return EmptyChunk
	}
	d.Chk.NoError(err)

	return NewChunkWithHash(ref, data)
}

func (l *internalLevelDBStore) hasByKey(key []byte) bool {
	exists, err := l.db.Has(key, &opt.ReadOptions{DontFillCache: true}) // This isn't really a "read", so don't signal the cache to treat it as one.
	d.Chk.NoError(err)
	l.hasCount++
	return exists
}

func (l *internalLevelDBStore) putByKey(key []byte, c Chunk) {
	l.concurrentWriteLimit <- struct{}{}
	err := l.db.Put(key, c.Data(), nil)
	d.Chk.NoError(err)
	l.putCount++
	l.putBytes += int64(len(c.Data()))
	<-l.concurrentWriteLimit
}

func (l *internalLevelDBStore) putBatch(b *leveldb.Batch, numBytes int) {
	l.concurrentWriteLimit <- struct{}{}
	err := l.db.Write(b, nil)
	d.Chk.NoError(err)
	l.putCount += int64(b.Len())
	l.putBytes += int64(numBytes)
	<-l.concurrentWriteLimit
}

func (l *internalLevelDBStore) Close() error {
	l.db.Close()
	if l.dumpStats {
		fmt.Println("--LevelDB Stats--")
		fmt.Println("GetCount: ", l.getCount)
		fmt.Println("HasCount: ", l.hasCount)
		fmt.Println("PutCount: ", l.putCount)
		fmt.Println("Average PutSize: ", l.putBytes/l.putCount)
	}
	return nil
}

func NewLevelDBStoreFactory(dir string, maxHandles int, dumpStats bool) Factory {
	return &LevelDBStoreFactory{dir, maxHandles, dumpStats, newBackingStore(dir, maxHandles, dumpStats)}
}

func NewLevelDBStoreFactoryUseFlags(dir string) Factory {
	return NewLevelDBStoreFactory(dir, ldbFlags.maxFileHandles, ldbFlags.dumpStats)
}

type LevelDBStoreFactory struct {
	dir            string
	maxFileHandles int
	dumpStats      bool
	store          *internalLevelDBStore
}

func (f *LevelDBStoreFactory) CreateStore(ns string) ChunkStore {
	d.Chk.NotNil(f.store, "Cannot use LevelDBStoreFactory after Shutter().")
	return newLevelDBStore(f.store, []byte(ns), false)
}

func (f *LevelDBStoreFactory) Shutter() {
	f.store.Close()
	f.store = nil
}
