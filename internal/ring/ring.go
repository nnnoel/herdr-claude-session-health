// Package ring provides a fixed-capacity FIFO buffer used to keep every
// in-memory history, log and alert list bounded.
package ring

// Ring is a fixed-capacity FIFO buffer that overwrites its oldest element
// once full. The zero value is not usable; construct it with New.
type Ring[T any] struct {
	buf   []T
	start int
	n     int
}

// New returns an empty ring holding at most capacity elements (minimum 1).
func New[T any](capacity int) *Ring[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring[T]{buf: make([]T, capacity)}
}

// FromSlice builds a ring of the given capacity holding the newest elements of items.
func FromSlice[T any](capacity int, items []T) *Ring[T] {
	r := New[T](capacity)
	if len(items) > r.Cap() {
		items = items[len(items)-r.Cap():]
	}
	for _, item := range items {
		r.Push(item)
	}
	return r
}

// Cap returns the maximum number of elements.
func (r *Ring[T]) Cap() int { return len(r.buf) }

// Len returns the number of stored elements.
func (r *Ring[T]) Len() int { return r.n }

// Push appends v, evicting the oldest element when full.
func (r *Ring[T]) Push(v T) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = v
		r.n++
		return
	}
	r.buf[r.start] = v
	r.start = (r.start + 1) % len(r.buf)
}

// At returns the i-th element, oldest first. It panics when i is out of range.
func (r *Ring[T]) At(i int) T {
	if i < 0 || i >= r.n {
		panic("ring: index out of range")
	}
	return r.buf[(r.start+i)%len(r.buf)]
}

// Last returns the newest element.
func (r *Ring[T]) Last() (T, bool) {
	var zero T
	if r.n == 0 {
		return zero, false
	}
	return r.At(r.n - 1), true
}

// Slice copies the elements, oldest first.
func (r *Ring[T]) Slice() []T {
	out := make([]T, r.n)
	for i := range out {
		out[i] = r.At(i)
	}
	return out
}
