# Const types

This toolchain implements an experimental language extension: **const types**.
A const type `const T` is a deeply read-only view of a `T`. The compiler (and
`go vet`, `gofmt`, `go doc`, and `go/types`) understand const types; the
qualifier is checked statically and is erased before code generation, so it
has no run-time cost and no effect on representation, layout, or ABI.

The goals are to

- eliminate defensive copies: an API can hand out internal slices, maps, and
  pointers as `const` and callers cannot modify them; and
- find bugs: accidental modification of data that was only meant to be read
  becomes a compile-time error.

```go
type Registry struct {
	hosts []string
	byName map[string]*Entry
}

// Before: func (r *Registry) Hosts() []string { return slices.Clone(r.hosts) }
func (r const *Registry) Hosts() const []string           { return r.hosts }
func (r const *Registry) Lookup(n string) const *Entry    { return r.byName[n] }

func use(r const *Registry) {
	hs := r.Hosts()
	fmt.Println(len(hs), hs[0]) // ok: reading
	hs[0] = "x"                 // error: cannot assign to hs[0] (read-only: accessed through const value)
	r.Lookup("a").Weight++      // error: read-only
	r.hosts = nil               // error: read-only
}
```

## Syntax

```
Type          = TypeName | TypeLit | ConstType | "(" Type ")" .
ConstType     = "const" Type .
ParameterDecl = [ IdentifierList ] [ [ "const" ] "..." ] Type .
MethodElem    = [ "const" ] MethodName Signature .
```

`const` binds to the whole type that follows: `const []*T` is a const slice
(of, transitively, const pointers). `const const T` is `const T`. Const types
may be used wherever a type may be used: parameters, results, receivers,
variables, struct fields, element types (`[]const *T`, `map[K]const *V`,
`chan const *T`), type arguments, conversions (`(const []int)(s)`), and alias
declarations (`type View = const *Config`). A defined type cannot have a const
type as its underlying type; use an alias.

A statement that begins with `const` is still a constant declaration.

## Semantics

**Read-only, transitively.** Any location reached through a value of const
type is read-only, and what is read from it is itself viewed through a const
type:

| expression (x of const type) | result                                   |
|------------------------------|------------------------------------------|
| `x[i]`, `x.f`, `*x`          | read-only; type `const E`                 |
| `x[i:j]`                     | `const []E`                              |
| `&x[i]`, `&x.f`              | `const *E`                               |
| `for k, v := range x`        | `k`, `v` have const types                |
| `x.(T)`, type switch on x    | `const T`                                |

Read-only locations cannot be assigned to (including `++`, `op=`), and the
built-ins `append`, `copy` (as destination), `clear`, and `delete` reject
const operands.

**Value types are unaffected.** If `T` contains no references (basic types,
strings, and arrays and structs of such types; function and channel values are
opaque to const and are treated the same way), then a `const T` *value* is
simply a `T`: copying it yields an independent value. So `n := c.count` gives
an ordinary `int`, while `c.count = 1` is still rejected because the *location*
is read-only.

**Variables of const type can be reassigned.** `const` qualifies what a value
refers to, not the variable holding it (like `const T*` in C, not `T* const`):

```go
var p const *Node = head
for ; p != nil; p = p.next { ... } // ok
```

**Assignability.** A `T` (or `const T`) is assignable to `const T`. A
`const T` is assignable to `T` only if `T` is a value type; otherwise the
assignment "discards const qualifier". This applies uniformly to assignment,
argument passing, returns, sends, composite literals, and conversions; in
particular a const value cannot be converted to a non-const interface such as
`any` (use `const any`). Under a const type, inner const qualifiers are
redundant: `const []*T` and `const []const *T` are mutually assignable.
`copy(dst, src)` and `append(dst, src...)` accept a const `src` if the element
copies do not discard const (`[]int` from `const []int` is fine; `[]*T` from
`const []*T` is not, but `[]const *T` is).

**Const methods.** A method may declare a const receiver, and an interface
method may be declared const. Only const methods can be called on const
values (methods with a non-pointer receiver of a value type are also
callable, since they operate on an independent copy):

```go
func (b const *Buffer) Len() int { return len(b.buf) } // const method
func (b *Buffer) Reset()         { b.buf = b.buf[:0] }

type Sized interface {
	const Len() int // callable on a const Sized
	Reset()
}
```

A type implements an interface's const method only with a const method. The
method set of `const T` consists of T's const methods.

**Const-compatible signatures.** A function may be used where a function type
is expected, and a method may implement an interface method, if the signatures
differ only in const qualifiers and the function promises at least as much as
is required: its parameters may be const where the expected ones are not, and
the expected results may be const where its results are not. So adding `const`
to a parameter is a backward-compatible change:

```go
func (w *W) Write(p const []byte) (int, error) // still implements io.Writer
var f func([]byte, []byte) bool = bytes.Equal   // bytes.Equal takes const []byte
```

**Const variadic parameters.** `func Printf(format string, a const ...any)`
declares `a` with type `const []any`; each argument may be const. A `[]any`
can still be passed with `args...`.

**Generics.** Const types can be type arguments (`Stack[const *T]`,
`iter.Seq[const *T]`). A const type does not satisfy a `~[]E`-style
constraint; generic functions that only read should declare const parameters
instead, which accept both const and non-const arguments:

```go
func Index[S ~[]E, E comparable](s const S, v E) int
```

## Standard library

- `fmt` (`Print*`, `Sprint*`, `Fprint*`, `Append*`, `Errorf`), `log`, and
  `testing` (`Log`, `Error`, `Fatal`, `Skip` and their `f` variants) accept
  const operands.
- `slices.Index`, `slices.Contains`, `slices.Equal`, `bytes.Equal`,
  `bytes.Clone`, `(*bytes.Buffer).Write`, and `(*strings.Builder).Write` take
  const slices.
- `go/ast` has a new `ConstType` node; `go/types` has a new `ConstType` type,
  `NewConstType`, `IdenticalIgnoreConst`, and `(*Signature).ConstMethod` /
  `SetConstMethod`. A variadic signature's final parameter may have type
  `const []T`. Addressable read-only operands report `Addressable() == true`
  and `Assignable() == false`.

## What const does not guarantee

Const is a static discipline on *views*, comparable to `const` in C/C++ or
`readonly` in TypeScript:

- It is not immutability: the owner of the data (or anyone holding a non-const
  reference) can still modify it, and a const view observes the change.
- `unsafe` removes it: `*(*[]int)(unsafe.Pointer(&cs))`, or
  `(*T)(unsafe.Pointer(cp))`. This is the intended escape hatch.
- Dynamic types ignore const. A top-level const value cannot be stored in a
  non-const interface, but const qualifiers nested inside another type
  (`[]const *T`, `G[const *T]`, a type parameter instantiated with a const
  type) do not exist at run time, so a round trip through `any` and a type
  assertion (or reflection) can recover the non-const type. Likewise, a
  dynamic interface-to-interface assertion cannot check that a method is const.
- Functions and channels are opaque: calling a function value or receiving
  from a channel reached through a const value yields whatever the function
  or channel provides.
- `fmt` treats its operands as read-only by contract but invokes user-defined
  `String`, `Error`, and `Format` methods, and `fmt.Errorf("%w", err)` retains
  `err`.

## Implementation

- `cmd/compile/internal/syntax`: `ConstType` node; parser and printer.
- `cmd/compile/internal/types2` and `go/types` (kept in sync through the
  generator): the `ConstType` type (`consttype.go`), a read-only addressable
  operand mode, and the rules above in assignability, conversion, selectors,
  index/slice/range expressions, built-ins, method lookup and interface
  satisfaction, unification, and type inference.
- `cmd/compile/internal/noder`: const types are written to export data (a new
  type code, and a const flag per interface method appended to the interface
  encoding) and erased when the backend reads types.
- `go/ast`, `go/parser`, `go/printer`, `go/doc`, `go/internal/gcimporter`, and
  the vendored `golang.org/x/tools` analyzers used by `go vet` and `go fix`.

Tests: `internal/types/testdata/check/consttypes.go` (both type checkers),
`test/consttype.go`, `test/consttype1.go`, and `test/consttype2.go`.
