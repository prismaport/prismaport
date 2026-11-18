package service

// Field is one optional member of a create or patch body: whether the request sent it, whether
// it sent null, and its value otherwise. What null means is the input's business: on a patch it
// usually clears the member; elsewhere it is the same as leaving the member out.
type Field[T any] struct {
	Set   bool
	Null  bool
	Value T
}

// Set is a Field sent with value v.
func Set[T any](v T) Field[T] { return Field[T]{Set: true, Value: v} }

// Given reports whether the request sent a value (neither left the member out nor sent null).
func (f Field[T]) Given() bool { return f.Set && !f.Null }

// Ptr is the value when one was given, nil otherwise.
func (f Field[T]) Ptr() *T {
	if !f.Given() {
		return nil
	}
	v := f.Value
	return &v
}

// patchText reads an optional text member the way a product takes it: nil when absent, "" when
// it clears the member (null on a patch, or ""), the text otherwise. On a create null is absent.
func patchText(f Field[string], creating bool) *string {
	if f.Set && f.Null && !creating {
		empty := ""
		return &empty
	}
	return f.Ptr()
}
