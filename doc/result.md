# Result unwrapping: error-propagating selectors

This fork of Go adds one experimental language feature: a selector
expression may be applied directly to a multi-valued `(T, error)`
expression. The selector operates on the `T` value; if the error is
non-nil, the enclosing function returns it immediately.

```go
func size(name string) (int64, error) {
	return os.Open(name).Stat().Size(), nil
}
```

is exactly equivalent to

```go
func size(name string) (int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
```

The idea is that `(T, error)` already *is* Go's `Result[T, error]`: it is a
real type in the type checker (a two-element tuple), it just cannot be
stored or named. Applying `.` to it is the operation that was missing:
unwrap the `Ok` value, or propagate the `Err` value to the caller, much like
Rust's `?` operator.

The feature is always enabled in this fork. The normative description is in
the language specification, [doc/go_spec.html](go_spec.html), section
"Selectors on multi-value expressions".

## Semantics

Given a selector expression `x.f` whose operand `x` is a multi-valued
expression (in practice, a call) of type `(T, error)`:

1. `x.f` is only valid inside a function (including a function literal)
   whose **final result has type `error`**. Anywhere else, including at
   package level, it is a compile-time error.
2. `x.f` denotes the field or method `f` of a **non-addressable value of
   type `T`**: the same rules that apply to `g().f` for a single-valued
   call `g()`. In particular, a pointer-receiver method cannot be called
   on an unwrapped value of non-pointer type, and `x.f` cannot be assigned
   to.
3. At run time, `x` is evaluated at its normal place in the left-to-right
   evaluation order. If the error value is non-nil, the enclosing function
   returns at once: the error is its final result and **all other results
   are their zero values**, including named results. Deferred functions run
   as for any other `return` statement. Otherwise, `f` is selected from the
   first value and evaluation continues.
4. Only expressions of exactly two values whose second value is exactly
   the predeclared type `error` can be unwrapped. `(T, bool)`,
   `(T, *MyError)`, `(T, U, error)` and so on are rejected with the usual
   "multiple-value in single-value context" error.

Because selector expressions on multi-valued operands were always a
compile-time error, no existing program changes meaning. The grammar is
unchanged: `gofmt`, `go/parser` and `go/ast` need no modification.

Chaining works naturally, since a method call on an unwrapped value may
itself return `(U, error)`:

```go
func parse(src string) (ast.Node, error) {
	return newParser(src).parseFile().Decls[0], nil
}
```

## Design choices, and where this deviates from the original idea

The original idea was "`(T, err)` should be the same as `Result[T, err]` from
the type system's perspective, and `t.foo(x)` on a `Result` should call `foo`
on the `T` value, or return the error". The implementation keeps the core
idea but makes some deliberate choices:

**No nameable `Result[T, E]` type.** `(T, error)` is only ever the type of a
call expression; tuples are not first-class values in Go. Adding a named
generic `Result` type would mean inventing tuple-typed variables,
conversions between `(T, error)` and `Result[T, error]`, composite literals,
and so on, which is a far larger change to the language and to every tool.
The selector rule needs none of that: the type checker already represents a
two-result call as a `*types.Tuple`, and the rule simply lets a selector see
through it. The "Result type" therefore exists as a rule about an existing
type rather than as a new type.

**Exactly `error`, on both sides.** The second value must have the
predeclared type `error`, and the enclosing function's last result must be
`error` too, rather than "anything implementing `error`" or "anything
`error` is assignable to" (such as `any`). This keeps the rule easy to state
and predict, and it avoids the classic nil-interface trap: propagating a
`*MyError` into an `error` result would make a nil `*MyError` turn into a
non-nil `error`. Both restrictions are easy to relax later if experience
shows they are too tight.

**Zero values for the other results.** When an error propagates, every other
result is set to its zero value, even if it is a named result that already
holds something. This mirrors Rust's `Err(e)`, which carries nothing else,
and the overwhelmingly common Go idiom `return nil, err`. A deferred
function can still inspect and adjust named results, so patterns such as
"wrap the error on the way out" keep working:

```go
func render(w io.Writer, src string, data any) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("render: %w", err)
		}
	}()
	return template.New("page").Parse(src).Execute(w, data)
}
```

**Not addressable.** The unwrapped value behaves like any other function
result. Making it an addressable temporary would have been more permissive
(`f().PtrMethod()` on a value `T`) but would have introduced a difference
between `g().M()` and `f().M()` that is hard to justify.

**Evaluation order is preserved.** The error check happens exactly where the
unwrap is evaluated, not hoisted to the start of the statement. In
`a() + f().N + b()`, if `f` fails, `a` has run and `b` has not.

**The dot is the only trigger.** A value with no fields or methods cannot be
unwrapped, so `strconv.Atoi(s)` still needs the explicit two-line form, and
there is no way to write "unwrap and pass along" such as `g(f(x)?)`. This is
the main ergonomic gap of the dot-based design. A postfix operator (`f(x)?`,
or a built-in `try(f(x))`) would be more general, but it is a new syntax,
with new questions about precedence and formatting; this experiment
deliberately stays within the existing grammar so that the cost of the
feature can be judged on its own.

**Always on, no `GOEXPERIMENT`.** The whole fork is the experiment. Gating
the feature behind a `GOEXPERIMENT` flag would be a few lines in
`unwrapResult` (see below), but it would also have to be threaded through
`go vet` and `go/types`, which do not see the compiler's experiment flags.

## Implementation

The change is small and lives in three places.

**Type checking** (`src/cmd/compile/internal/types2/result.go`, mirrored in
`src/go/types/result.go`). `Checker.selector` used to evaluate its operand
with `exprOrType`, which rejects multi-valued operands. It now evaluates the
operand with `rawExpr` and calls `Checker.unwrapResult`, which, for a
`(T, error)` operand inside a suitable function, turns the operand into a
value of type `T`. Everything downstream (field and method lookup,
`Info.Selections`, `Info.Types`) then works unchanged. `Info.Types` records
the tuple type for the operand, which is how the compiler front end
recognizes an unwrap. The mirror in `go/types` is what makes `go vet` (which
runs during `go test`) and `gopls` accept the construct.

**Unified IR writer** (`src/cmd/compile/internal/noder/writer.go`). When a
selector's operand has a tuple type, `writer.selectorRecv` writes a new
expression code, `exprUnwrap`, in front of the operand.

**Unified IR reader** (`src/cmd/compile/internal/noder/reader.go`).
`reader.unwrapExpr` lowers the unwrap to an `OINLCALL` node, the same
statement-carrying expression node that the inliner produces:

```go
v, err := x            // fresh temporaries
if err != nil {
	return zero, ..., err
}
v                      // the result of the OINLCALL
```

Later passes already know how to handle `OINLCALL`: the order pass emits
the body at the point where the expression is evaluated (which is what
preserves left-to-right evaluation order), escape analysis walks the body,
and when a function containing an unwrap is itself inlined, the existing
rewrite of `return` statements into assignments and a `goto` applies to the
generated `return` too, including across packages via export data. No
changes were needed to `walk`, SSA generation, or the inliner.

## Tests

- `test/unwrap.go`: run-time behaviour (fields, methods, chaining,
  embedded fields, named results, defer, many result types, evaluation
  order, method values, closures, generics, use in every kind of statement).
- `test/unwrap1.go`: the compile-time errors.
- `test/unwrap2.go` (+ `test/unwrap2.dir`): cross-package use and inlining
  of functions containing unwraps.

Run them with

```
cd src && go test cmd/internal/testdir -run='Test/unwrap'
```

The type-checker suites (`go test go/types cmd/compile/internal/types2`)
and the full `test/` directory pass, and the standard library and commands
build unchanged with the modified compiler.

## Possible follow-ups

- Relax the `error` restriction to any interface type that `error` is
  assignable to (for the enclosing function) and any type implementing
  `error` (for the operand), with a nil check appropriate to the type.
- A bare unwrap for values without fields or methods, for example a postfix
  operator, if the selector form proves useful but too narrow.
- Error wrapping: the `defer`-based pattern above works today; a dedicated
  handler construct could be layered on later without changing this rule.
