package solver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/tracing"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// ResolveOpFunc finds an Op implementation for a Vertex
type ResolveOpFunc func(Vertex, Builder) (Op, error)

type Builder interface {
	Build(ctx context.Context, e Edge) (CachedResult, error)
	Call(ctx context.Context, name string, fn func(ctx context.Context) error) error
}

// JobList provides a shared graph of all the vertexes currently being
// processed. Every vertex that is being solved needs to be loaded into job
// first. Vertex operations are invoked and progress tracking happends through
// jobs.
// TODO: s/JobList/Solver
type JobList struct {
	mu      sync.RWMutex
	jobs    map[string]*Job
	actives map[digest.Digest]*state
	opts    SolverOpt

	updateCond *sync.Cond
	s          *Scheduler
	index      *EdgeIndex
}

type state struct {
	jobs     map[*Job]struct{}
	parents  map[digest.Digest]struct{}
	childVtx map[digest.Digest]struct{}

	mpw   *progress.MultiWriter
	allPw map[progress.Writer]struct{}

	vtx          Vertex
	clientVertex client.Vertex

	mu    sync.Mutex
	op    *sharedOp
	edges map[Index]*edge
	opts  SolverOpt
	index *EdgeIndex

	cache     map[string]CacheManager
	mainCache CacheManager
	jobList   *JobList
}

func (s *state) getSessionID() string {
	// TODO: connect with sessionmanager to avoid getting dropped sessions
	s.mu.Lock()
	for j := range s.jobs {
		if j.SessionID != "" {
			s.mu.Unlock()
			return j.SessionID
		}
	}
	parents := map[digest.Digest]struct{}{}
	for p := range s.parents {
		parents[p] = struct{}{}
	}
	s.mu.Unlock()

	for p := range parents {
		s.jobList.mu.Lock()
		pst, ok := s.jobList.actives[p]
		s.jobList.mu.Unlock()
		if ok {
			if sessionID := pst.getSessionID(); sessionID != "" {
				return sessionID
			}
		}
	}
	return ""
}

func (s *state) builder() *subBuilder {
	return &subBuilder{state: s}
}

func (s *state) getEdge(index Index) *edge {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.edges[index]; ok {
		return e
	}

	if s.op == nil {
		s.op = newSharedOp(s.opts.ResolveOpFunc, s.opts.DefaultCache, s)
	}

	e := newEdge(Edge{Index: index, Vertex: s.vtx}, s.op, s.index)
	s.edges[index] = e
	return e
}

func (s *state) setEdge(index Index, newEdge *edge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.edges[index]
	if ok {
		if e == newEdge {
			return
		}
		e.release()
	}

	newEdge.incrementReferenceCount()
	s.edges[index] = newEdge
}

func (s *state) combinedCacheManager() CacheManager {
	s.mu.Lock()
	cms := make([]CacheManager, 0, len(s.cache)+1)
	cms = append(cms, s.mainCache)
	for _, cm := range s.cache {
		cms = append(cms, cm)
	}
	s.mu.Unlock()

	if len(cms) == 1 {
		return s.mainCache
	}

	return newCombinedCacheManager(cms, s.mainCache)
}

func (s *state) Release() {
	for _, e := range s.edges {
		e.release()
	}
	if s.op != nil {
		s.op.release()
	}
}

type subBuilder struct {
	*state
	mu        sync.Mutex
	exporters []ExportableCacheKey
}

func (sb *subBuilder) Build(ctx context.Context, e Edge) (CachedResult, error) {
	res, err := sb.jobList.subBuild(ctx, e, sb.vtx)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	sb.exporters = append(sb.exporters, res.CacheKey())
	sb.mu.Unlock()
	return res, nil
}

func (sb *subBuilder) Call(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	ctx = progress.WithProgress(ctx, sb.mpw)
	return inVertexContext(ctx, name, fn)
}

type Job struct {
	list *JobList
	pr   *progress.MultiReader
	pw   progress.Writer

	progressCloser func()
	SessionID      string
}

type SolverOpt struct {
	ResolveOpFunc ResolveOpFunc
	DefaultCache  CacheManager
}

func NewJobList(opts SolverOpt) *JobList {
	if opts.DefaultCache == nil {
		opts.DefaultCache = NewInMemoryCacheManager()
	}
	jl := &JobList{
		jobs:    make(map[string]*Job),
		actives: make(map[digest.Digest]*state),
		opts:    opts,
		index:   NewEdgeIndex(),
	}
	jl.s = NewScheduler(jl)
	jl.updateCond = sync.NewCond(jl.mu.RLocker())
	return jl
}

func (jl *JobList) SetEdge(e Edge, newEdge *edge) {
	jl.mu.RLock()
	defer jl.mu.RUnlock()

	st, ok := jl.actives[e.Vertex.Digest()]
	if !ok {
		return
	}

	st.setEdge(e.Index, newEdge)
}

func (jl *JobList) GetEdge(e Edge) *edge {
	jl.mu.RLock()
	defer jl.mu.RUnlock()

	st, ok := jl.actives[e.Vertex.Digest()]
	if !ok {
		return nil
	}
	return st.getEdge(e.Index)
}

func (jl *JobList) subBuild(ctx context.Context, e Edge, parent Vertex) (CachedResult, error) {
	v, err := jl.load(e.Vertex, parent, nil)
	if err != nil {
		return nil, err
	}
	e.Vertex = v
	return jl.s.build(ctx, e)
}

func (jl *JobList) Close() {
	jl.s.Stop()
}

func (jl *JobList) load(v, parent Vertex, j *Job) (Vertex, error) {
	jl.mu.Lock()
	defer jl.mu.Unlock()

	cache := map[Vertex]Vertex{}

	return jl.loadUnlocked(v, parent, j, cache)
}

func (jl *JobList) loadUnlocked(v, parent Vertex, j *Job, cache map[Vertex]Vertex) (Vertex, error) {
	if v, ok := cache[v]; ok {
		return v, nil
	}
	origVtx := v

	inputs := make([]Edge, len(v.Inputs()))
	for i, e := range v.Inputs() {
		v, err := jl.loadUnlocked(e.Vertex, parent, j, cache)
		if err != nil {
			return nil, err
		}
		inputs[i] = Edge{Index: e.Index, Vertex: v}
	}

	dgst := v.Digest()

	dgstWithoutCache := digest.FromBytes([]byte(fmt.Sprintf("%s-ignorecache", dgst)))

	// if same vertex is already loaded without cache just use that
	st, ok := jl.actives[dgstWithoutCache]

	if !ok {
		st, ok = jl.actives[dgst]

		// !ignorecache merges with ignorecache but ignorecache doesn't merge with !ignorecache
		if ok && !st.vtx.Options().IgnoreCache && v.Options().IgnoreCache {
			dgst = dgstWithoutCache
		}

		v = &vertexWithCacheOptions{
			Vertex: v,
			dgst:   dgst,
			inputs: inputs,
		}

		st, ok = jl.actives[dgst]
	}

	if !ok {
		st = &state{
			opts:         jl.opts,
			jobs:         map[*Job]struct{}{},
			parents:      map[digest.Digest]struct{}{},
			childVtx:     map[digest.Digest]struct{}{},
			allPw:        map[progress.Writer]struct{}{},
			mpw:          progress.NewMultiWriter(progress.WithMetadata("vertex", dgst)),
			vtx:          v,
			clientVertex: initClientVertex(v),
			edges:        map[Index]*edge{},
			index:        jl.index,
			mainCache:    jl.opts.DefaultCache,
			cache:        map[string]CacheManager{},
			jobList:      jl,
		}
		jl.actives[dgst] = st
	}

	st.mu.Lock()
	if cache := v.Options().CacheSource; cache != nil && cache.ID() != st.mainCache.ID() {
		st.cache[cache.ID()] = cache
	}

	if j != nil {
		if _, ok := st.jobs[j]; !ok {
			st.jobs[j] = struct{}{}
		}
	}
	st.mu.Unlock()

	if parent != nil {
		if _, ok := st.parents[parent.Digest()]; !ok {
			st.parents[parent.Digest()] = struct{}{}
			parentState, ok := jl.actives[parent.Digest()]
			if !ok {
				return nil, errors.Errorf("inactive parent %s", parent.Digest())
			}
			parentState.childVtx[dgst] = struct{}{}

			for id, c := range parentState.cache {
				st.cache[id] = c
			}
		}
	}

	jl.connectProgressFromState(st, st)
	cache[origVtx] = v
	return v, nil
}

func (jl *JobList) connectProgressFromState(target, src *state) {
	for j := range src.jobs {
		if _, ok := target.allPw[j.pw]; !ok {
			target.mpw.Add(j.pw)
			target.allPw[j.pw] = struct{}{}
			j.pw.Write(target.clientVertex.Digest.String(), target.clientVertex)
		}
	}
	for p := range src.parents {
		jl.connectProgressFromState(target, jl.actives[p])
	}
}

func (jl *JobList) NewJob(id string) (*Job, error) {
	jl.mu.Lock()
	defer jl.mu.Unlock()

	if _, ok := jl.jobs[id]; ok {
		return nil, errors.Errorf("job ID %s exists", id)
	}

	pr, ctx, progressCloser := progress.NewContext(context.Background())
	pw, _, _ := progress.FromContext(ctx) // TODO: expose progress.Pipe()

	j := &Job{
		list:           jl,
		pr:             progress.NewMultiReader(pr),
		pw:             pw,
		progressCloser: progressCloser,
	}
	jl.jobs[id] = j

	jl.updateCond.Broadcast()

	return j, nil
}

func (jl *JobList) Get(id string) (*Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		<-ctx.Done()
		jl.updateCond.Broadcast()
	}()

	jl.mu.RLock()
	defer jl.mu.RUnlock()
	for {
		select {
		case <-ctx.Done():
			return nil, errors.Errorf("no such job %s", id)
		default:
		}
		j, ok := jl.jobs[id]
		if !ok {
			jl.updateCond.Wait()
			continue
		}
		return j, nil
	}
}

// called with joblist lock
func (jl *JobList) deleteIfUnreferenced(k digest.Digest, st *state) {
	if len(st.jobs) == 0 && len(st.parents) == 0 {
		for chKey := range st.childVtx {
			chState := jl.actives[chKey]
			delete(chState.parents, k)
			jl.deleteIfUnreferenced(chKey, chState)
		}
		st.Release()
		delete(jl.actives, k)
	}
}

func (j *Job) Build(ctx context.Context, e Edge) (CachedResult, error) {
	v, err := j.list.load(e.Vertex, nil, j)
	if err != nil {
		return nil, err
	}
	e.Vertex = v
	return j.list.s.build(ctx, e)
}

func (j *Job) Discard() error {
	defer j.progressCloser()

	j.list.mu.Lock()
	defer j.list.mu.Unlock()

	j.pw.Close()

	for k, st := range j.list.actives {
		if _, ok := st.jobs[j]; ok {
			delete(st.jobs, j)
			j.list.deleteIfUnreferenced(k, st)
		}
		if _, ok := st.allPw[j.pw]; ok {
			delete(st.allPw, j.pw)
		}
	}
	return nil
}

func (j *Job) Call(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	ctx = progress.WithProgress(ctx, j.pw)
	return inVertexContext(ctx, name, fn)
}

type activeOp interface {
	CacheMap(context.Context) (*CacheMap, error)
	LoadCache(ctx context.Context, rec *CacheRecord) (Result, error)
	Exec(ctx context.Context, inputs []Result) (outputs []Result, exporters []ExportableCacheKey, err error)
	IgnoreCache() bool
	Cache() CacheManager
	CalcSlowCache(context.Context, Index, ResultBasedCacheFunc, Result) (digest.Digest, error)
}

func newSharedOp(resolver ResolveOpFunc, cacheManager CacheManager, st *state) *sharedOp {
	so := &sharedOp{
		resolver:     resolver,
		st:           st,
		slowCacheRes: map[Index]digest.Digest{},
		slowCacheErr: map[Index]error{},
	}
	return so
}

type execRes struct {
	execRes       []*SharedResult
	execExporters []ExportableCacheKey
}

type sharedOp struct {
	resolver ResolveOpFunc
	st       *state
	g        flightcontrol.Group

	opOnce     sync.Once
	op         Op
	subBuilder *subBuilder
	err        error

	execRes *execRes
	execErr error

	cacheRes *CacheMap
	cacheErr error

	slowMu       sync.Mutex
	slowCacheRes map[Index]digest.Digest
	slowCacheErr map[Index]error
}

func (s *sharedOp) IgnoreCache() bool {
	return s.st.vtx.Options().IgnoreCache
}

func (s *sharedOp) Cache() CacheManager {
	return s.st.combinedCacheManager()
}

func (s *sharedOp) LoadCache(ctx context.Context, rec *CacheRecord) (Result, error) {
	ctx = progress.WithProgress(ctx, s.st.mpw)
	// no cache hit. start evaluating the node
	span, ctx := tracing.StartSpan(ctx, "load cache: "+s.st.vtx.Name())
	notifyStarted(ctx, &s.st.clientVertex, true)
	res, err := s.Cache().Load(ctx, rec)
	tracing.FinishWithError(span, err)
	notifyCompleted(ctx, &s.st.clientVertex, err, true)
	return res, err
}

func (s *sharedOp) CalcSlowCache(ctx context.Context, index Index, f ResultBasedCacheFunc, res Result) (digest.Digest, error) {
	key, err := s.g.Do(ctx, fmt.Sprintf("slow-compute-%d", index), func(ctx context.Context) (interface{}, error) {
		s.slowMu.Lock()
		// TODO: add helpers for these stored values
		if res := s.slowCacheRes[index]; res != "" {
			s.slowMu.Unlock()
			return res, nil
		}
		if err := s.slowCacheErr[index]; err != nil {
			s.slowMu.Unlock()
			return err, nil
		}
		s.slowMu.Unlock()
		ctx = progress.WithProgress(ctx, s.st.mpw)
		key, err := f(ctx, res)
		complete := true
		if err != nil {
			canceled := false
			select {
			case <-ctx.Done():
				canceled = true
			default:
			}
			if canceled && errors.Cause(err) == context.Canceled {
				complete = false
			}
		}
		s.slowMu.Lock()
		defer s.slowMu.Unlock()
		if complete {
			if err == nil {
				s.slowCacheRes[index] = key
			}
			s.slowCacheErr[index] = err
		}
		return key, err
	})
	if err != nil {
		return "", err
	}
	return key.(digest.Digest), nil
}

func (s *sharedOp) CacheMap(ctx context.Context) (*CacheMap, error) {
	op, err := s.getOp()
	if err != nil {
		return nil, err
	}
	res, err := s.g.Do(ctx, "cachemap", func(ctx context.Context) (ret interface{}, retErr error) {
		if s.cacheRes != nil {
			return s.cacheRes, nil
		}
		if s.cacheErr != nil {
			return nil, s.cacheErr
		}
		ctx = progress.WithProgress(ctx, s.st.mpw)
		ctx = session.NewContext(ctx, s.st.getSessionID())
		if len(s.st.vtx.Inputs()) == 0 {
			// no cache hit. start evaluating the node
			span, ctx := tracing.StartSpan(ctx, "cache request: "+s.st.vtx.Name())
			notifyStarted(ctx, &s.st.clientVertex, false)
			defer func() {
				tracing.FinishWithError(span, retErr)
				notifyCompleted(ctx, &s.st.clientVertex, retErr, false)
			}()
		}
		res, err := op.CacheMap(ctx)
		complete := true
		if err != nil {
			canceled := false
			select {
			case <-ctx.Done():
				canceled = true
			default:
			}
			if canceled && errors.Cause(err) == context.Canceled {
				complete = false
			}
		}
		if complete {
			if err == nil {
				s.cacheRes = res
			}
			s.cacheErr = err
		}
		return res, err
	})
	if err != nil {
		return nil, err
	}
	return res.(*CacheMap), nil
}

func (s *sharedOp) Exec(ctx context.Context, inputs []Result) (outputs []Result, exporters []ExportableCacheKey, err error) {
	op, err := s.getOp()
	if err != nil {
		return nil, nil, err
	}
	res, err := s.g.Do(ctx, "exec", func(ctx context.Context) (ret interface{}, retErr error) {
		if s.execRes != nil || s.execErr != nil {
			return s.execRes, s.execErr
		}

		ctx = progress.WithProgress(ctx, s.st.mpw)
		ctx = session.NewContext(ctx, s.st.getSessionID())

		// no cache hit. start evaluating the node
		span, ctx := tracing.StartSpan(ctx, s.st.vtx.Name())
		notifyStarted(ctx, &s.st.clientVertex, false)
		defer func() {
			tracing.FinishWithError(span, retErr)
			notifyCompleted(ctx, &s.st.clientVertex, retErr, false)
		}()

		res, err := op.Exec(ctx, inputs)
		complete := true
		if err != nil {
			canceled := false
			select {
			case <-ctx.Done():
				canceled = true
			default:
			}
			if canceled && errors.Cause(err) == context.Canceled {
				complete = false
			}
		}
		if complete {
			if res != nil {
				var subExporters []ExportableCacheKey
				s.subBuilder.mu.Lock()
				if len(s.subBuilder.exporters) > 0 {
					subExporters = append(subExporters, s.subBuilder.exporters...)
				}
				s.subBuilder.mu.Unlock()

				s.execRes = &execRes{execRes: wrapShared(res), execExporters: subExporters}
			}
			s.execErr = err
		}
		return s.execRes, err
	})
	if err != nil {
		return nil, nil, err
	}
	r := res.(*execRes)
	return unwrapShared(r.execRes), r.execExporters, nil
}

func (s *sharedOp) getOp() (Op, error) {
	s.opOnce.Do(func() {
		s.subBuilder = s.st.builder()
		s.op, s.err = s.resolver(s.st.vtx, s.subBuilder)
	})
	if s.err != nil {
		return nil, s.err
	}
	return s.op, nil
}

func (s *sharedOp) release() {
	if s.execRes != nil {
		for _, r := range s.execRes.execRes {
			r.Release(context.TODO())
		}
	}
}

func initClientVertex(v Vertex) client.Vertex {
	inputDigests := make([]digest.Digest, 0, len(v.Inputs()))
	for _, inp := range v.Inputs() {
		inputDigests = append(inputDigests, inp.Vertex.Digest())
	}
	return client.Vertex{
		Inputs: inputDigests,
		Name:   v.Name(),
		Digest: v.Digest(),
	}
}

func wrapShared(inp []Result) []*SharedResult {
	out := make([]*SharedResult, len(inp))
	for i, r := range inp {
		out[i] = NewSharedResult(r)
	}
	return out
}

func unwrapShared(inp []*SharedResult) []Result {
	out := make([]Result, len(inp))
	for i, r := range inp {
		out[i] = r.Clone()
	}
	return out
}

type vertexWithCacheOptions struct {
	Vertex
	inputs []Edge
	dgst   digest.Digest
}

func (v *vertexWithCacheOptions) Digest() digest.Digest {
	return v.dgst
}

func (v *vertexWithCacheOptions) Inputs() []Edge {
	return v.inputs
}

func notifyStarted(ctx context.Context, v *client.Vertex, cached bool) {
	pw, _, _ := progress.FromContext(ctx)
	defer pw.Close()
	now := time.Now()
	v.Started = &now
	v.Completed = nil
	v.Cached = cached
	pw.Write(v.Digest.String(), *v)
}

func notifyCompleted(ctx context.Context, v *client.Vertex, err error, cached bool) {
	pw, _, _ := progress.FromContext(ctx)
	defer pw.Close()
	now := time.Now()
	if v.Started == nil {
		v.Started = &now
	}
	v.Completed = &now
	v.Cached = cached
	if err != nil {
		v.Error = err.Error()
	}
	pw.Write(v.Digest.String(), *v)
}

func inVertexContext(ctx context.Context, name string, f func(ctx context.Context) error) error {
	v := client.Vertex{
		Digest: digest.FromBytes([]byte(identity.NewID())),
		Name:   name,
	}
	pw, _, ctx := progress.FromContext(ctx, progress.WithMetadata("vertex", v.Digest))
	notifyStarted(ctx, &v, false)
	defer pw.Close()
	err := f(ctx)
	notifyCompleted(ctx, &v, err, false)
	return err
}
