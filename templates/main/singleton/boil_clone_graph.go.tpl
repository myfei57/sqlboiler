{{- if .CloneGraph }}
// CloneGraph deep-copies loaded model structs and the relationships already
// present in their R structs.
//
// The generated code never executes SQL, never invokes loaders or hooks and
// never round-trips values through JSON serialization. Only data that is
// already loaded is copied: a nil relationship pointer stays nil, a nil slice
// stays nil and an empty non-nil slice stays an empty non-nil slice, and the
// mere presence of a foreign key value never triggers a lookup.
//
// The L loader structs, executors, query objects and the insert/update caches
// are deliberately unreachable from a CloneGraphSession: they are runtime
// machinery, not graph data, so they are never copied. Generated model
// structs carry no projection state; primary key values are copied verbatim
// and nothing marks a clone as a new database record.

// CloneGraphBudget bounds the work performed by a CloneGraphSession.
//
// The zero value (every field <= 0) means "unlimited" for that dimension.
// When a limit is exceeded the session fails as a whole: Clone* returns the
// budget error together with a nil result, so a partially cloned graph can
// never escape and the source graph is left untouched.
type CloneGraphBudget struct {
	// MaxNodes limits the number of model structs allocated.
	MaxNodes int
	// MaxEdges limits the number of relationship pointers followed
	// (each to-many slice element counts as one edge).
	MaxEdges int
	// MaxDepth limits the longest relationship path measured from a root,
	// roots are at depth 0.
	MaxDepth int
	// MaxBytes limits the cumulative number of bytes charged for copying
	// mutable leaves (byte slices, maps, slices, decimals, ...).
	MaxBytes int
}

// CloneGraphStats reports the resources consumed by a session so far.
type CloneGraphStats struct {
	Nodes    int
	Edges    int
	MaxDepth int
	Bytes    int
}

// CloneGraphError wraps a budget violation.
type CloneGraphError struct {
	Op    string
	Limit int
	Value int
}

func (e *CloneGraphError) Error() string {
	return fmt.Sprintf("clonegraph: %s budget exceeded (limit %d, reached %d)", e.Op, e.Limit, e.Value)
}

// Unwrap exposes the sentinel so errors.Is works against budget failures.
func (e *CloneGraphError) Unwrap() error {
	return ErrCloneGraphBudgetExceeded
}

var (
	// ErrCloneGraphBudgetExceeded is the sentinel wrapped by CloneGraphError
	// when a budget dimension is exceeded.
	ErrCloneGraphBudgetExceeded = errors.New("clonegraph: budget exceeded")
	// ErrCloneGraphSessionFailed is returned when a session is reused after an
	// earlier clone already failed; failed sessions keep their partial memo
	// and must not be reused.
	ErrCloneGraphSessionFailed = errors.New("clonegraph: session has failed")
	// ErrCloneGraphAdapterMissing is returned when a type-replaced column has
	// no adapter registered through RegisterCloneGraphAdapter.
	ErrCloneGraphAdapterMissing = errors.New("clonegraph: no clone adapter registered for type")
)

// cloneGraphJob is one unit of iterative graph traversal. All traversal goes
// through an explicit job queue instead of recursion, so arbitrarily long
// relationship chains cannot overflow the goroutine stack.
type cloneGraphJob struct {
	depth int
	fn    func() error
}

// cloneGraphLeafKey identifies a mutable source value (slice/map/pointer) by
// both its address and its type. The type is part of the key because zero
// length slices of different element types can share the runtime zero base
// address; mixing them in one address-only memo would panic on retrieval.
type cloneGraphLeafKey struct {
	typ reflect.Type
	ptr uintptr
}

func cloneGraphLeafKeyOf(v any) cloneGraphLeafKey {
	rv := reflect.ValueOf(v)
	return cloneGraphLeafKey{typ: rv.Type(), ptr: rv.Pointer()}
}

// CloneGraphSession keeps the identity memos of one clone run. Several roots
// can be cloned through the same session: any model, slice, map or byte
// backing array reachable from more than one root is cloned exactly once and
// every reference to the source resolves to the same cloned value, preserving
// shared leaves and cycles. Sessions are not safe for concurrent use.
type CloneGraphSession struct {
	budget CloneGraphBudget
	failed bool

	jobs []cloneGraphJob

	nodes    int
	edges    int
	maxDepth int
	bytes    int

	// leafMemo memoizes mutable non-model values keyed by source identity,
	// so equal-but-distinct instances are still copied independently while
	// genuinely shared backing memory stays shared in the clone.
	leafMemo map[cloneGraphLeafKey]any

	// adapters is a snapshot of the globally registered adapters taken when
	// the session is created, so later registrations never affect a run.
	adapters *sync.Map

	{{- range .Tables }}
	{{- if not .IsJoinTable }}
	{{- $alias := $.Aliases.Table .Name }}
	{{$alias.DownSingular}}Memo map[*{{$alias.UpSingular}}]*{{$alias.UpSingular}}
	{{- end }}
	{{- end }}
}

// globalCloneGraphAdapters holds the adapters registered by package users.
var globalCloneGraphAdapters sync.Map

// RegisterCloneGraphAdapter registers an independent deep-copy function for a
// type used through a type replacement. Registration is expected to happen
// during package initialization, before any CloneGraphSession is created.
//
// The adapter receives a source value and must return a value whose mutable
// state cannot be reached from the source; returning a shallow copy violates
// the CloneGraph contract. It must not perform SQL or trigger side effects.
func RegisterCloneGraphAdapter[T any](fn func(T) (T, error)) {
	var zero T
	globalCloneGraphAdapters.Store(reflect.TypeOf(zero), func(src any) (any, error) {
		return fn(src.(T))
	})
}

// NewCloneGraphSession creates a session bounded by budget. Pass the zero
// CloneGraphBudget for unlimited cloning.
func NewCloneGraphSession(budget CloneGraphBudget) *CloneGraphSession {
	adapters := &sync.Map{}
	globalCloneGraphAdapters.Range(func(k, v any) bool {
		adapters.Store(k, v)
		return true
	})

	return &CloneGraphSession{
		budget:   budget,
		leafMemo: make(map[cloneGraphLeafKey]any),
		adapters: adapters,

		{{- range .Tables }}
		{{- if not .IsJoinTable }}
		{{- $alias := $.Aliases.Table .Name }}
		{{$alias.DownSingular}}Memo: make(map[*{{$alias.UpSingular}}]*{{$alias.UpSingular}}),
		{{- end }}
		{{- end }}
	}
}

// Stats returns the resources consumed by the session.
func (s *CloneGraphSession) Stats() CloneGraphStats {
	return CloneGraphStats{
		Nodes:    s.nodes,
		Edges:    s.edges,
		MaxDepth: s.maxDepth,
		Bytes:    s.bytes,
	}
}

// checkFailed makes sure a failed session is never reused.
func (s *CloneGraphSession) checkFailed() error {
	if s.failed {
		return ErrCloneGraphSessionFailed
	}
	return nil
}

func (s *CloneGraphSession) noteDepth(depth int) {
	if depth > s.maxDepth {
		s.maxDepth = depth
	}
}

func (s *CloneGraphSession) budgetError(op string, limit, value int) error {
	s.failed = true
	return &CloneGraphError{Op: op, Limit: limit, Value: value}
}

// chargeNode accounts for one allocated model struct.
func (s *CloneGraphSession) chargeNode(depth int) error {
	s.nodes++
	s.noteDepth(depth)
	if s.budget.MaxNodes > 0 && s.nodes > s.budget.MaxNodes {
		return s.budgetError("node", s.budget.MaxNodes, s.nodes)
	}
	if s.budget.MaxDepth > 0 && depth > s.budget.MaxDepth {
		return s.budgetError("depth", s.budget.MaxDepth, depth)
	}
	return nil
}

// chargeEdge accounts for one followed relationship pointer.
func (s *CloneGraphSession) chargeEdge(depth int) error {
	s.edges++
	s.noteDepth(depth)
	if s.budget.MaxEdges > 0 && s.edges > s.budget.MaxEdges {
		return s.budgetError("edge", s.budget.MaxEdges, s.edges)
	}
	if s.budget.MaxDepth > 0 && depth > s.budget.MaxDepth {
		return s.budgetError("depth", s.budget.MaxDepth, depth)
	}
	return nil
}

// chargeBytes accounts for n bytes of copied mutable content.
func (s *CloneGraphSession) chargeBytes(n int) error {
	s.bytes += n
	if s.budget.MaxBytes > 0 && s.bytes > s.budget.MaxBytes {
		return s.budgetError("byte", s.budget.MaxBytes, s.bytes)
	}
	return nil
}

// enqueue schedules a traversal job.
func (s *CloneGraphSession) enqueue(depth int, fn func() error) {
	s.jobs = append(s.jobs, cloneGraphJob{depth: depth, fn: fn})
}

// run drains the job queue iteratively. Any error marks the session failed
// and is returned to the entry point, which reports a nil result.
func (s *CloneGraphSession) run() error {
	for len(s.jobs) > 0 {
		job := s.jobs[0]
		s.jobs = s.jobs[1:]

		if err := job.fn(); err != nil {
			s.failed = true
			return err
		}
	}
	return nil
}

// cloneGraphSlice returns an independent copy of src while preserving nil
// slices and sharing identity inside one session: two fields pointing at the
// same source backing array resolve to the same cloned array.
func cloneGraphSlice[T any](s *CloneGraphSession, src []T) ([]T, error) {
	if src == nil {
		return nil, nil
	}

	key := cloneGraphLeafKeyOf(src)
	if cached, ok := s.leafMemo[key]; ok {
		return cached.([]T), nil
	}

	dst := make([]T, len(src))
	copy(dst, src)

	if err := s.chargeBytes(len(src) * int(reflect.TypeOf(src).Elem().Size())); err != nil {
		return nil, err
	}

	s.leafMemo[key] = dst
	return dst, nil
}

// cloneGraphJSON copies defined []byte types such as types.JSON, keeping
// their concrete type and their nil identity.
func cloneGraphJSON[B ~[]byte](s *CloneGraphSession, src B) (B, error) {
	if src == nil {
		return nil, nil
	}

	cp, err := cloneGraphSlice(s, []byte(src))
	if err != nil {
		return nil, err
	}
	return B(cp), nil
}

// cloneGraphDefinedSlice copies a named slice type such as types.Int64Array
// or a []pgeo.Point, keeping the concrete named type and the nil identity.
func cloneGraphDefinedSlice[S ~[]E, E any](s *CloneGraphSession, src S) (S, error) {
	if src == nil {
		return nil, nil
	}

	cp, err := cloneGraphSlice(s, []E(src))
	if err != nil {
		return nil, err
	}
	return S(cp), nil
}

// cloneGraphDefinedMap copies a named map type such as types.HStore, keeping
// the concrete named type and the nil identity.
func cloneGraphDefinedMap[M ~map[K]V, K comparable, V any](s *CloneGraphSession, src M) (M, error) {
	if src == nil {
		return nil, nil
	}

	cp, err := cloneGraphMap(s, map[K]V(src))
	if err != nil {
		return nil, err
	}
	return M(cp), nil
}

// cloneGraphMap returns an independent copy of src, preserving nil maps and
// session-wide identity sharing. Map values must be value types so a plain
// assignment copies them fully.
func cloneGraphMap[K comparable, V any](s *CloneGraphSession, src map[K]V) (map[K]V, error) {
	if src == nil {
		return nil, nil
	}

	key := cloneGraphLeafKeyOf(src)
	if cached, ok := s.leafMemo[key]; ok {
		return cached.(map[K]V), nil
	}

	dst := make(map[K]V, len(src))
	weight := len(src) * int(reflect.TypeOf(src).Key().Size()+reflect.TypeOf(src).Elem().Size())
	for k, v := range src {
		dst[k] = v
		if str, ok := any(k).(string); ok {
			weight += len(str)
		}
	}

	if err := s.chargeBytes(weight); err != nil {
		return nil, err
	}

	s.leafMemo[key] = dst
	return dst, nil
}

// cloneGraphBytesArray deep-copies a slice of byte slices: the outer slice,
// every inner []byte and nil inner elements all keep their original identity.
func cloneGraphBytesArray(s *CloneGraphSession, src [][]byte) ([][]byte, error) {
	if src == nil {
		return nil, nil
	}

	key := cloneGraphLeafKeyOf(src)
	if cached, ok := s.leafMemo[key]; ok {
		return cached.([][]byte), nil
	}

	dst := make([][]byte, len(src))
	weight := len(src) * int(reflect.TypeOf(src).Elem().Size())
	for i := range src {
		cp, err := cloneGraphSlice(s, src[i])
		if err != nil {
			return nil, err
		}
		dst[i] = cp
		weight += len(cp)
	}

	if err := s.chargeBytes(weight); err != nil {
		return nil, err
	}

	s.leafMemo[key] = dst
	return dst, nil
}

// cloneGraphDefinedBytesArray copies a named [][]byte type such as
// types.BytesArray, keeping the concrete named type and the nil identity.
func cloneGraphDefinedBytesArray[S ~[][]byte](s *CloneGraphSession, src S) (S, error) {
	if src == nil {
		return nil, nil
	}

	cp, err := cloneGraphBytesArray(s, [][]byte(src))
	if err != nil {
		return nil, err
	}
	return S(cp), nil
}

// cloneGraphLeafOnce runs build once per (source pointer, result type) pair
// within the session, memoizing its result and charging its byte weight. It
// is used by generated code for pointer-wrapped values such as the
// *decimal.Big inside decimals. The result type is part of the identity: two
// different wrappers (for example types.Decimal and types.NullDecimal) may
// share one inner pointer, but they never share a memoized cloned value.
func cloneGraphLeafOnce[T any](s *CloneGraphSession, ptr uintptr, build func() (T, int)) (T, error) {
	key := cloneGraphLeafKey{typ: reflect.TypeOf(*new(T)), ptr: ptr}
	if cached, ok := s.leafMemo[key]; ok {
		return cached.(T), nil
	}

	dst, weight := build()
	if err := s.chargeBytes(weight); err != nil {
		var zero T
		return zero, err
	}

	s.leafMemo[key] = dst
	return dst, nil
}

// cloneGraphAdapter resolves the user-registered deep-copy function for the
// dynamic type of T. Missing adapters fail the session instead of falling
// back to a shallow copy.
func cloneGraphAdapter[T any](s *CloneGraphSession, src T) (T, error) {
	var zero T
	typ := reflect.TypeOf(zero)

	v, ok := s.adapters.Load(typ)
	if !ok {
		return zero, fmt.Errorf("%w: %s", ErrCloneGraphAdapterMissing, typ.String())
	}

	cp, err := v.(func(any) (any, error))(src)
	if err != nil {
		return zero, err
	}
	return cp.(T), nil
}
{{ end -}}
