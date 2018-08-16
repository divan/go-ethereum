// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package storage

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/swarm/log"

	lru "github.com/hashicorp/golang-lru"
)

type (
	NewNetFetcherFunc func(ctx context.Context, addr Address, peers *sync.Map) NetFetcher
)

type NetFetcher interface {
	Request(ctx context.Context)
	Offer(ctx context.Context, source *discover.NodeID)
}

type WithNetStoreId interface {
	SetNetstoreId(id int)
}

// NetStore is an extension of local storage
// it implements the ChunkStore interface
// on request it initiates remote cloud retrieval using a fetcher
// fetchers are unique to a chunk and are stored in fetchers LRU memory cache
// fetchFuncFactory is a factory object to create a fetch function for a specific chunk address
type NetStore struct {
	mu                sync.Mutex
	store             ChunkStore
	fetchers          *lru.Cache
	NewNetFetcherFunc NewNetFetcherFunc
	id                int
}

// NewNetStore creates a new NetStore object using the given local store. newFetchFunc is a
// constructor function that can create a fetch function for a specific chunk address.
func NewNetStore(store ChunkStore, nnf NewNetFetcherFunc) (*NetStore, error) {
	fetchers, err := lru.New(defaultChunkRequestsCacheCapacity)
	if err != nil {
		return nil, err
	}

	return &NetStore{
		store:             store,
		fetchers:          fetchers,
		NewNetFetcherFunc: nnf,
		id:                rand.Intn(10000),
	}, nil
}

// Put stores a chunk in localstore, and delivers to all requestor peers using the fetcher stored in
// the fetchers cache
func (n *NetStore) Put(ctx context.Context, ch Chunk) error {
	log.Warn("Chunk is put", "addr", ch.Address(), "netstore", n.id)
	n.mu.Lock()
	defer n.mu.Unlock()

	// chunk, err := n.store.Get(ctx, ch.Address())
	// if chunk != nil {
	// 	return nil
	// }

	// put to the chunk to the store, there should be no error
	err := n.store.Put(ctx, ch)
	if err != nil {
		return err
	}

	// if chunk is now put in the store, check if there was an active fetcher and call deliver on it
	// (this delivers the chunk to requestors via the fetcher)
	if f := n.getFetcher(ch.Address()); f != nil {
		f.deliver(ctx, ch)
	}
	return nil
}

// Get retrieves the chunk from the NetStore DPA synchronously.
// It calls NetStore.get, and if the chunk is not in local Storage
// it calls fetch with the request, which blocks until the chunk
// arrived or context is done
func (n *NetStore) Get(rctx context.Context, ref Address) (Chunk, error) {
	chunk, fetch, err := n.get(rctx, ref)
	if fetch == nil {
		return chunk, err
	}
	return fetch(rctx)
}

// Has returns nil if the store contains the given address. Otherwise it returns a wait function,
// which returns after the chunk is available or the context is done
func (n *NetStore) Has(ctx context.Context, ref Address) func(context.Context) error {
	chunk, fetch, _ := n.get(ctx, ref)
	if chunk != nil {
		return nil
	}
	return func(ctx context.Context) error {
		_, err := fetch(ctx)
		return err
	}
}

// Close chunk store
func (n *NetStore) Close() {
	n.store.Close()
}

// get attempts at retrieving the chunk from LocalStore
// If it is not found then using getOrCreateFetcher:
//     1. Either there is already a fetcher to retrieve it
//     2. A new fetcher is created and saved in the fetchers cache
// From here on, all Get will hit on this fetcher until the chunk is delivered
// or all fetcher contexts are done.
// It returns a chunk, a fetcher function and an error
// If chunk is nil, the returned fetch function needs to be called with a context to return the chunk.
func (n *NetStore) get(ctx context.Context, ref Address) (Chunk, func(context.Context) (Chunk, error), error) {
	log.Warn("Chunk is get", "addr", ref, "netstore", n.id)
	n.mu.Lock()
	defer n.mu.Unlock()

	chunk, err := n.store.Get(ctx, ref)
	if err == nil {
		// The chunk is available in the LocalStore, so the returned fetch function is not necessary.
		// However, we still return a fetch function which immediately returns the same chunk again if called.
		return chunk, func(context.Context) (Chunk, error) { return chunk, nil }, nil
	}
	// The chunk is not available in the LocalStore, let's get the fetcher for it, or create a new one
	// if it doesn't exist yet
	f := n.getOrCreateFetcher(ref)
	// If the caller needs the chunk, it has to use the returned fetch function to get it
	return nil, f.Fetch, err
}

// getOrCreateFetcher attempts at retrieving an existing fetchers
// if none exists, creates one and saves it in the fetchers cache
// caller must hold the lock
func (n *NetStore) getOrCreateFetcher(ref Address) *fetcher {
	if f := n.getFetcher(ref); f != nil {
		return f
	}

	// no fetcher for the given address, we have to create a new one
	key := hex.EncodeToString(ref)
	// create the context during which fetching is kept alive
	ctx, cancel := context.WithCancel(context.Background())
	// destroy is called when all requests finish
	destroy := func() {
		// remove fetcher from fetchers
		n.fetchers.Remove(key)
		// stop fetcher by cancelling context called when
		// all requests cancelled/timedout or chunk is delivered
		cancel()
	}
	// peers always stores all the peers which have an active request for the chunk. It is shared
	// between fetcher and the NewFetchFunc function. It is needed by the NewFetchFunc because
	// the peers which requested the chunk should not be requested to deliver it.
	peers := &sync.Map{}

	fetcher := newFetcher(ref, n.NewNetFetcherFunc(ctx, ref, peers), destroy, peers, n.id)
	n.fetchers.Add(key, fetcher)

	return fetcher
}

// getFetcher retrieves the fetcher for the given address from the fetchers cache if it exists,
// otherwise it returns nil
func (n *NetStore) getFetcher(ref Address) *fetcher {
	key := hex.EncodeToString(ref)
	f, ok := n.fetchers.Get(key)
	if ok {
		return f.(*fetcher)
	}
	return nil
}

// RequestsCacheLen returns the current number of outgoing requests stored in the cache
func (n *NetStore) RequestsCacheLen() int {
	return n.fetchers.Len()
}

// One fetcher object is responsible to fetch one chunk for one address, and keep track of all the
// peers who have requested it and did not receive it yet.
type fetcher struct {
	addr       Address       // address of chunk
	chunk      Chunk         // fetcher can set the chunk on the fetcher
	deliveredC chan struct{} // chan signalling chunk delivery to requests
	// cancelledC  chan struct{} // chan signalling the fetcher has been cancelled (removed from fetchers in NetStore)
	netFetcher  NetFetcher // remote fetch function to be called with a request source taken from the context
	cancel      func()     // cleanup function for the remote fetcher to call when all upstream contexts are called
	peers       *sync.Map  // the peers which asked for the chunk
	requestCnt  int32      // number of requests on this chunk. If all the requests are done (delivered or context is done) the cancel function is called
	deliverOnce *sync.Once
	id          int
	netstoreId  int
}

// newFetcher creates a new fetcher object for the fiven addr. fetch is the function which actually
// does the retrieval (in non-test cases this is coming from the network package). cancel function is
// called either
//     1. when the chunk has been fetched all peers have been either notified or their context has been done
//     2. the chunk has not been fetched but all context from all the requests has been done
// The peers map stores all the peers which have requested chunk.
func newFetcher(addr Address, nf NetFetcher, cancel func(), peers *sync.Map, netstoreId int) *fetcher {
	// cancelOnce := &sync.Once{}        // cancel should only be called once
	// cancelledC := make(chan struct{}) // closed when fetcher is cancelled
	log.Warn("Fetcher is created for chunk", "addr", addr)
	nf.(WithNetStoreId).SetNetstoreId(netstoreId)
	return &fetcher{
		addr:        addr,
		deliveredC:  make(chan struct{}),
		deliverOnce: &sync.Once{},
		// cancelledC:  cancelledC,
		netFetcher: nf,
		cancel:     cancel,
		peers:      peers,
		id:         rand.Intn(10000),
		netstoreId: netstoreId,
	}
}

// Fetch fetches the chunk synchronously, it is called by NetStore.Get is the chunk is not available
// locally.
func (f *fetcher) Fetch(rctx context.Context) (Chunk, error) {
	atomic.AddInt32(&f.requestCnt, 1)
	defer func() {
		// if all the requests are done the fetcher can be cancelled
		if atomic.AddInt32(&f.requestCnt, -1) == 0 {
			f.cancel()
		}
	}()

	// The peer asking for the chunk. Store in the shared peers map, but delete after the request
	// has been delivered
	peer := rctx.Value("peer")
	if peer != nil {
		f.peers.Store(peer, true)
		defer f.peers.Delete(peer)
	}

	// If there is a source in the context then it is an offer, otherwise a request
	sourceIF := rctx.Value("source")
	if sourceIF != nil {
		var source *discover.NodeID
		id := discover.MustHexID(sourceIF.(string))
		source = &id
		log.Warn("Fetcher is doing an offer", "addr", f.addr, "fetcher", f.id, "netstore", f.netstoreId)
		f.netFetcher.Offer(rctx, source)
	} else {
		log.Warn("Fetcher is doing a request", "addr", f.addr, "fetcher", f.id, "netstore", f.netstoreId)
		f.netFetcher.Request(rctx)
	}

	// wait until either the chunk is delivered or the context is done
	log.Warn("Fetcher is waiting for put", "addr", f.addr, "fetcher", f.id, "netstore", f.netstoreId)
	select {
	case <-rctx.Done():
		log.Warn("Fetcher timeout", "addr", f.addr, "fetcher", f.id, "netstore", f.netstoreId)
		return nil, fmt.Errorf("context deadline exceeded, addr %v, netstore %v", f.addr, f.netstoreId)
	case <-f.deliveredC:
		log.Warn("Fetcher is done", "addr", f.addr, "fetcher", f.id, "netstore", f.netstoreId)
		return f.chunk, nil
	}
}

// deliver is called by NetStore.Put to notify all pending requests
func (f *fetcher) deliver(ctx context.Context, ch Chunk) {
	f.deliverOnce.Do(func() {
		f.chunk = ch
		// closing the deliveredC channel will terminate ongoing requests
		close(f.deliveredC)
	})
}

// SyncNetStore is a wrapped NetStore with SyncDB functionality
type SyncNetStore struct {
	store SyncChunkStore
	*NetStore
}

func NewSyncNetStore(store SyncChunkStore, nnf NewNetFetcherFunc) (*SyncNetStore, error) {
	netStore, err := NewNetStore(store, nnf)
	if err != nil {
		return nil, err
	}
	return &SyncNetStore{
		store:    store,
		NetStore: netStore,
	}, nil
}

func (sn *SyncNetStore) BinIndex(po uint8) uint64 {
	return sn.store.BinIndex(po)
}

func (sn *SyncNetStore) Iterator(from uint64, to uint64, po uint8, f func(Address, uint64) bool) error {
	return sn.store.Iterator(from, to, po, f)
}
