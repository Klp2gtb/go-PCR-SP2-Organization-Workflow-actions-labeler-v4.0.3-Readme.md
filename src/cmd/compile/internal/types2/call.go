// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of call and selector expressions.

package types2

import (
	"cmd/compile/internal/syntax"
	"fmt"
	. "internal/types/errors"
	"strings"
	"unicode"
)

// funcInst type-checks a function instantiation and returns the result in x.
// The incoming x must be an uninstantiated generic function. If inst != nil,
// it provides (some or all of) the type arguments (inst.Index) for the
// instantiation. If the target type tsig != nil, the signature's parameter
// types are used to infer additional missing type arguments of x, if any.
// At least one of tsig or inst must be provided.
func (check *Checker) funcInst(tsig *Signature, pos syntax.Pos, x *operand, inst *syntax.IndexExpr) {
	assert(tsig != nil || inst != nil)

	if !check.allowVersion(check.pkg, pos, 1, 18) {
		var posn poser
		if inst != nil {
			posn = inst.Pos()
		} else {
			posn = pos
		}
		check.versionErrorf(posn, "go1.18", "function instantiation")
	}

	// targs and xlist are the type arguments and corresponding type expressions, or nil.
	var targs []Type
	var xlist []syntax.Expr
	if inst != nil {
		xlist = unpackExpr(inst.Index)
		targs = check.typeList(xlist)
		if targs == nil {
			x.mode = invalid
			x.expr = inst
			return
		}
		assert(len(targs) == len(xlist))
	}

	// Check the number of type arguments (got) vs number of type parameters (want).
	// Note that x is a function value, not a type expression, so we don't need to
	// call under below.
	sig := x.typ.(*Signature)
	got, want := len(targs), sig.TypeParams().Len()
	if got > want {
		// Providing too many type arguments is always an error.
		check.errorf(xlist[got-1], WrongTypeArgCount, "got %d type arguments but want %d", got, want)
		x.mode = invalid
		x.expr = inst
		return
	}

	if got < want {
		// If the uninstantiated or partially instantiated function x is used in an
		// assignment (tsig != nil), use the respective function parameter and result
		// types to infer additional type arguments.
		var args []*operand
		var params []*Var
		if tsig != nil && sig.tparams != nil && tsig.params.Len() == sig.params.Len() && tsig.results.Len() == sig.results.Len() {
			// x is a generic function and the signature arity matches the target function.
			// To infer x's missing type arguments, treat the function assignment as a call
			// of a synthetic function f where f's parameters are the parameters and results
			// of x and where the arguments to the call of f are values of the parameter and
			// result types of x.
			n := tsig.params.Len()
			m := tsig.results.Len()
			args = make([]*operand, n+m)
			params = make([]*Var, n+m)
			for i := 0; i < n; i++ {
				lvar := tsig.params.At(i)
				lname := syntax.NewName(x.Pos(), paramName(lvar.name, i, "parameter"))
				args[i] = &operand{mode: value, expr: lname, typ: lvar.typ}
				params[i] = sig.params.At(i)
			}
			for i := 0; i < m; i++ {
				lvar := tsig.results.At(i)
				lname := syntax.NewName(x.Pos(), paramName(lvar.name, i, "result parameter"))
				args[n+i] = &operand{mode: value, expr: lname, typ: lvar.typ}
				params[n+i] = sig.results.At(i)
			}
		}

		// Note that NewTuple(params...) below is nil if len(params) == 0, as desired.
		targs = check.infer(pos, sig.TypeParams().list(), targs, NewTuple(params...), args)
		if targs == nil {
			// error was already reported
			x.mode = invalid
			x.expr = inst
			return
		}
		got = len(targs)
	}
	assert(got == want)

	// instantiate function signature
	sig = check.instantiateSignature(x.Pos(), sig, targs, xlist)
	assert(sig.TypeParams().Len() == 0) // signature is not generic anymore

	x.typ = sig
	x.mode = value
	// If we don't have an index expression, keep the existing expression of x.
	if inst != nil {
		x.expr = inst
	}
	check.recordInstance(x.expr, targs, sig)
}

func paramName(name string, i int, kind string) string {
	if name != "" {
		return name
	}
	return nth(i+1) + " " + kind
}

func nth(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}

func (check *Checker) instantiateSignature(pos syntax.Pos, typ *Signature, targs []Type, xlist []syntax.Expr) (res *Signature) {
	assert(check != nil)
	assert(len(targs) == typ.TypeParams().Len())

	if check.conf.Trace {
		check.trace(pos, "-- instantiating signature %s with %s", typ, targs)
		check.indent++
		defer func() {
			check.indent--
			check.trace(pos, "=> %s (under = %s)", res, res.Underlying())
		}()
	}

	inst := check.instance(pos, typ, targs, nil, check.context()).(*Signature)
	assert(len(xlist) <= len(targs))

	// verify instantiation lazily (was go.dev/issue/50450)
	check.later(func() {
		tparams := typ.TypeParams().list()
		if i, err := check.verify(pos, tparams, targs, check.context()); err != nil {
			// best position for error reporting
			pos := pos
			if i < len(xlist) {
				pos = syntax.StartPos(xlist[i])
			}
			check.softErrorf(pos, InvalidTypeArg, "%s", err)
		} else {
			check.mono.recordInstance(check.pkg, pos, tparams, targs, xlist)
		}
	}).describef(pos, "verify instantiation")

	return inst
}

func (check *Checker) callExpr(x *operand, call *syntax.CallExpr) exprKind {
	var inst *syntax.IndexExpr // function instantiation, if any
	if iexpr, _ := call.Fun.(*syntax.IndexExpr); iexpr != nil {
		if check.indexExpr(x, iexpr) {
			// Delay function instantiation to argument checking,
			// where we combine type and value arguments for type
			// inference.
			assert(x.mode == value)
			inst = iexpr
		}
		x.expr = iexpr
		check.record(x)
	} else {
		check.exprOrType(x, call.Fun, true)
	}
	// x.typ may be generic

	switch x.mode {
	case invalid:
		check.use(call.ArgList...)
		x.expr = call
		return statement

	case typexpr:
		// conversion
		check.nonGeneric(nil, x)
		if x.mode == invalid {
			return conversion
		}
		T := x.typ
		x.mode = invalid
		switch n := len(call.ArgList); n {
		case 0:
			check.errorf(call, WrongArgCount, "missing argument in conversion to %s", T)
		case 1:
			check.expr(nil, x, call.ArgList[0])
			if x.mode != invalid {
				if t, _ := under(T).(*Interface); t != nil && !isTypeParam(T) {
					if !t.IsMethodSet() {
						check.errorf(call, MisplacedConstraintIface, "cannot use interface %s in conversion (contains specific type constraints or is comparable)", T)
						break
					}
				}
				if call.HasDots {
					check.errorf(call.ArgList[0], BadDotDotDotSyntax, "invalid use of ... in conversion to %s", T)
					break
				}
				check.conversion(x, T)
			}
		default:
			check.use(call.ArgList...)
			check.errorf(call.ArgList[n-1], WrongArgCount, "too many arguments in conversion to %s", T)
		}
		x.expr = call
		return conversion

	case builtin:
		// no need to check for non-genericity here
		id := x.id
		if !check.builtin(x, call, id) {
			x.mode = invalid
		}
		x.expr = call
		// a non-constant result implies a function call
		if x.mode != invalid && x.mode != constant_ {
			check.hasCallOrRecv = true
		}
		return predeclaredFuncs[id].kind
	}

	// ordinary function/method call
	// signature may be generic
	cgocall := x.mode == cgofunc

	// a type parameter may be "called" if all types have the same signature
	sig, _ := coreType(x.typ).(*Signature)
	if sig == nil {
		check.errorf(x, InvalidCall, invalidOp+"cannot call non-function %s", x)
		x.mode = invalid
		x.expr = call
		return statement
	}

	// Capture wasGeneric before sig is potentially instantiated below.
	wasGeneric := sig.TypeParams().Len() > 0

	// evaluate type arguments, if any
	var xlist []syntax.Expr
	var targs []Type
	if inst != nil {
		xlist = unpackExpr(inst.Index)
		targs = check.typeList(xlist)
		if targs == nil {
			check.use(call.ArgList...)
			x.mode = invalid
			x.expr = call
			return statement
		}
		assert(len(targs) == len(xlist))

		// check number of type arguments (got) vs number of type parameters (want)
		got, want := len(targs), sig.TypeParams().Len()
		if got > want {
			check.errorf(xlist[want], WrongTypeArgCount, "got %d type arguments but want %d", got, want)
			check.use(call.ArgList...)
			x.mode = invalid
			x.expr = call
			return statement
		}

		// If sig is generic and all type arguments are provided, preempt function
		// argument type inference by explicitly instantiating the signature. This
		// ensures that we record accurate type information for sig, even if there
		// is an error checking its arguments (for example, if an incorrect number
		// of arguments is supplied).
		if got == want && want > 0 {
			if !check.allowVersion(check.pkg, x.Pos(), 1, 18) {
				check.versionErrorf(inst.Pos(), "go1.18", "function instantiation")
			}

			sig = check.instantiateSignature(inst.Pos(), sig, targs, xlist)
			assert(sig.TypeParams().Len() == 0) // signature is not generic anymore
			check.recordInstance(inst, targs, sig)

			// targs have been consumed; proceed with checking arguments of the
			// non-generic signature.
			targs = nil
			xlist = nil
		}
	}

	// evaluate arguments
	args := check.exprList(call.ArgList)
	sig = check.arguments(call, sig, targs, args, xlist)

	if wasGeneric && sig.TypeParams().Len() == 0 {
		// update the recorded type of call.Fun to its instantiated type
		check.recordTypeAndValue(call.Fun, value, sig, nil)
	}

	// determine result
	switch sig.results.Len() {
	case 0:
		x.mode = novalue
	case 1:
		if cgocall {
			x.mode = commaerr
		} else {
			x.mode = value
		}
		x.typ = sig.results.vars[0].typ // unpack tuple
	default:
		x.mode = value
		x.typ = sig.results
	}
	x.expr = call
	check.hasCallOrRecv = true

	// if type inference failed, a parametrized result must be invalidated
	// (operands cannot have a parametrized type)
	if x.mode == value && sig.TypeParams().Len() > 0 && isParameterized(sig.TypeParams().list(), x.typ) {
		x.mode = invalid
	}

	return statement
}

func (check *Checker) exprList(elist []syntax.Expr) (xlist []*operand) {
	switch len(elist) {
	case 0:
		// nothing to do
	case 1:
		xlist, _ = check.multiExpr(elist[0], false)
	default:
		// multiple (possibly invalid) values
		xlist = make([]*operand, len(elist))
		for i, e := range elist {
			var x operand
			check.expr(nil, &x, e)
			xlist[i] = &x
		}
	}
	return
}

// xlist is the list of type argument expressions supplied in the source code.
func (check *Checker) arguments(call *syntax.CallExpr, sig *Signature, targs []Type, args []*operand, xlist []syntax.Expr) (rsig *Signature) {
	rsig = sig

	// TODO(gri) try to eliminate this extra verification loop
	for _, a := range args {
		switch a.mode {
		case typexpr:
			check.errorf(a, NotAnExpr, "%s used as value", a)
			return
		case invalid:
			return
		}
	}

	// Function call argument/parameter count requirements
	//
	//               | standard call    | dotdotdot call |
	// --------------+------------------+----------------+
	// standard func | nargs == npars   | invalid        |
	// --------------+------------------+----------------+
	// variadic func | nargs >= npars-1 | nargs == npars |
	// --------------+------------------+----------------+

	nargs := len(args)
	npars := sig.params.Len()
	ddd := call.HasDots

	// set up parameters
	sigParams := sig.params // adjusted for variadic functions (may be nil for empty parameter lists!)
	adjusted := false       // indicates if sigParams is different from t.params
	if sig.variadic {
		if ddd {
			// variadic_func(a, b, c...)
			if len(call.ArgList) == 1 && nargs > 1 {
				// f()... is not permitted if f() is multi-valued
				//check.errorf(call.Ellipsis, "cannot use ... with %d-valued %s", nargs, call.ArgList[0])
				check.errorf(call, InvalidDotDotDot, "cannot use ... with %d-valued %s", nargs, call.ArgList[0])
				return
			}
		} else {
			// variadic_func(a, b, c)
			if nargs >= npars-1 {
				// Create custom parameters for arguments: keep
				// the first npars-1 parameters and add one for
				// each argument mapping to the ... parameter.
				vars := make([]*Var, npars-1) // npars > 0 for variadic functions
				copy(vars, sig.params.vars)
				last := sig.params.vars[npars-1]
				typ := last.typ.(*Slice).elem
				for len(vars) < nargs {
					vars = append(vars, NewParam(last.pos, last.pkg, last.name, typ))
				}
				sigParams = NewTuple(vars...) // possibly nil!
				adjusted = true
				npars = nargs
			} else {
				// nargs < npars-1
				npars-- // for correct error message below
			}
		}
	} else {
		if ddd {
			// standard_func(a, b, c...)
			//check.errorf(call.Ellipsis, "cannot use ... in call to non-variadic %s", call.Fun)
			check.errorf(call, NonVariadicDotDotDot, "cannot use ... in call to non-variadic %s", call.Fun)
			return
		}
		// standard_func(a, b, c)
	}

	// check argument count
	if nargs != npars {
		var at poser = call
		qualifier := "not enough"
		if nargs > npars {
			at = args[npars].expr // report at first extra argument
			qualifier = "too many"
		} else if nargs > 0 {
			at = args[nargs-1].expr // report at last argument
		}
		// take care of empty parameter lists represented by nil tuples
		var params []*Var
		if sig.params != nil {
			params = sig.params.vars
		}
		var err error_
		err.code = WrongArgCount
		err.errorf(at, "%s arguments in call to %s", qualifier, call.Fun)
		err.errorf(nopos, "have %s", check.typesSummary(operandTypes(args), false))
		err.errorf(nopos, "want %s", check.typesSummary(varTypes(params), sig.variadic))
		check.report(&err)
		return
	}

	// infer type arguments and instantiate signature if necessary
	if sig.TypeParams().Len() > 0 {
		if !check.allowVersion(check.pkg, call.Pos(), 1, 18) {
			if iexpr, _ := call.Fun.(*syntax.IndexExpr); iexpr != nil {
				check.versionErrorf(iexpr.Pos(), "go1.18", "function instantiation")
			} else {
				check.versionErrorf(call.Pos(), "go1.18", "implicit function instantiation")
			}
		}
		targs := check.infer(call.Pos(), sig.TypeParams().list(), targs, sigParams, args)
		if targs == nil {
			return // error already reported
		}

		// compute result signature
		rsig = check.instantiateSignature(call.Pos(), sig, targs, xlist)
		assert(rsig.TypeParams().Len() == 0) // signature is not generic anymore
		check.recordInstance(call.Fun, targs, rsig)

		// Optimization: Only if the parameter list was adjusted do we
		// need to compute it from the adjusted list; otherwise we can
		// simply use the result signature's parameter list.
		if adjusted {
			sigParams = check.subst(call.Pos(), sigParams, makeSubstMap(sig.TypeParams().list(), targs), nil, check.context()).(*Tuple)
		} else {
			sigParams = rsig.params
		}
	}

	// check arguments
	if len(args) > 0 {
		context := check.sprintf("argument to %s", call.Fun)
		for i, a := range args {
			check.assignment(a, sigParams.vars[i].typ, context)
		}
	}

	return
}

var cgoPrefixes = [...]string{
	"_Ciconst_",
	"_Cfconst_",
	"_Csconst_",
	"_Ctype_",
	"_Cvar_", // actually a pointer to the var
	"_Cfpvar_fp_",
	"_Cfunc_",
	"_Cmacro_", // function to evaluate the expanded expression
}

func (check *Checker) selector(x *operand, e *syntax.SelectorExpr, def *Named, wantType bool) {
	// these must be declared before the "goto Error" statements
	var (
		obj      Object
		index    []int
		indirect bool
	)

	sel := e.Sel.Value
	// If the identifier refers to a package, handle everything here
	// so we don't need a "package" mode for operands: package names
	// can only appear in qualified identifiers which are mapped to
	// selector expressions.
	if ident, ok := e.X.(*syntax.Name); ok {
		obj := check.lookup(ident.Value)
		if pname, _ := obj.(*PkgName); pname != nil {
			assert(pname.pkg == check.pkg)
			check.recordUse(ident, pname)
			pname.used = true
			pkg := pname.imported

			var exp Object
			funcMode := value
			if pkg.cgo {
				// cgo special cases C.malloc: it's
				// rewritten to _CMalloc and does not
				// support two-result calls.
				if sel == "malloc" {
					sel = "_CMalloc"
				} else {
					funcMode = cgofunc
				}
				for _, prefix := range cgoPrefixes {
					// cgo objects are part of the current package (in file
					// _cgo_gotypes.go). Use regular lookup.
					_, exp = check.scope.LookupParent(prefix+sel, check.pos)
					if exp != nil {
						break
					}
				}
				if exp == nil {
					check.errorf(e.Sel, UndeclaredImportedName, "undefined: %s", syntax.Expr(e)) // cast to syntax.Expr to silence vet
					goto Error
				}
				check.objDecl(exp, nil)
			} else {
				exp = pkg.scope.Lookup(sel)
				if exp == nil {
					if !pkg.fake {
						check.errorf(e.Sel, UndeclaredImportedName, "undefined: %s", syntax.Expr(e))
					}
					goto Error
				}
				if !exp.Exported() {
					check.errorf(e.Sel, UnexportedName, "%s not exported by package %s", sel, pkg.name)
					// ok to continue
				}
			}
			check.recordUse(e.Sel, exp)

			// Simplified version of the code for *syntax.Names:
			// - imported objects are always fully initialized
			switch exp := exp.(type) {
			case *Const:
				assert(exp.Val() != nil)
				x.mode = constant_
				x.typ = exp.typ
				x.val = exp.val
			case *TypeName:
				x.mode = typexpr
				x.typ = exp.typ
			case *Var:
				x.mode = variable
				x.typ = exp.typ
				if pkg.cgo && strings.HasPrefix(exp.name, "_Cvar_") {
					x.typ = x.typ.(*Pointer).base
				}
			case *Func:
				x.mode = funcMode
				x.typ = exp.typ
				if pkg.cgo && strings.HasPrefix(exp.name, "_Cmacro_") {
					x.mode = value
					x.typ = x.typ.(*Signature).results.vars[0].typ
				}
			case *Builtin:
				x.mode = builtin
				x.typ = exp.typ
				x.id = exp.id
			default:
				check.dump("%v: unexpected object %v", posFor(e.Sel), exp)
				unreachable()
			}
			x.expr = e
			return
		}
	}

	check.exprOrType(x, e.X, false)
	switch x.mode {
	case typexpr:
		// don't crash for "type T T.x" (was go.dev/issue/51509)
		if def != nil && x.typ == def {
			check.cycleError([]Object{def.obj})
			goto Error
		}
	case builtin:
		check.errorf(e.Pos(), UncalledBuiltin, "cannot select on %s", x)
		goto Error
	case invalid:
		goto Error
	}

	// Avoid crashing when checking an invalid selector in a method declaration
	// (i.e., where def is not set):
	//
	//   type S[T any] struct{}
	//   type V = S[any]
	//   func (fs *S[T]) M(x V.M) {}
	//
	// All codepaths below return a non-type expression. If we get here while
	// expecting a type expression, it is an error.
	//
	// See go.dev/issue/57522 for more details.
	//
	// TODO(rfindley): We should do better by refusing to check selectors in all cases where
	// x.typ is incomplete.
	if wantType {
		check.errorf(e.Sel, NotAType, "%s is not a type", syntax.Expr(e))
		goto Error
	}

	obj, index, indirect = LookupFieldOrMethod(x.typ, x.mode == variable, check.pkg, sel)
	if obj == nil {
		// Don't report another error if the underlying type was invalid (go.dev/issue/49541).
		if under(x.typ) == Typ[Invalid] {
			goto Error
		}

		if index != nil {
			// TODO(gri) should provide actual type where the conflict happens
			check.errorf(e.Sel, AmbiguousSelector, "ambiguous selector %s.%s", x.expr, sel)
			goto Error
		}

		if indirect {
			if x.mode == typexpr {
				check.errorf(e.Sel, InvalidMethodExpr, "invalid method expression %s.%s (needs pointer receiver (*%s).%s)", x.typ, sel, x.typ, sel)
			} else {
				check.errorf(e.Sel, InvalidMethodExpr, "cannot call pointer method %s on %s", sel, x.typ)
			}
			goto Error
		}

		var why string
		if isInterfacePtr(x.typ) {
			why = check.interfacePtrError(x.typ)
		} else {
			why = check.sprintf("type %s has no field or method %s", x.typ, sel)
			// Check if capitalization of sel matters and provide better error message in that case.
			// TODO(gri) This code only looks at the first character but LookupFieldOrMethod has an
			//           (internal) mechanism for case-insensitive lookup. Should use that instead.
			if len(sel) > 0 {
				var changeCase string
				if r := rune(sel[0]); unicode.IsUpper(r) {
					changeCase = string(unicode.ToLower(r)) + sel[1:]
				} else {
					changeCase = string(unicode.ToUpper(r)) + sel[1:]
				}
				if obj, _, _ = LookupFieldOrMethod(x.typ, x.mode == variable, check.pkg, changeCase); obj != nil {
					why += ", but does have " + changeCase
				}
			}
		}
		check.errorf(e.Sel, MissingFieldOrMethod, "%s.%s undefined (%s)", x.expr, sel, why)
		goto Error
	}

	// methods may not have a fully set up signature yet
	if m, _ := obj.(*Func); m != nil {
		check.objDecl(m, nil)
	}

	if x.mode == typexpr {
		// method expression
		m, _ := obj.(*Func)
		if m == nil {
			// TODO(gri) should check if capitalization of sel matters and provide better error message in that case
			check.errorf(e.Sel, MissingFieldOrMethod, "%s.%s undefined (type %s has no method %s)", x.expr, sel, x.typ, sel)
			goto Error
		}

		check.recordSelection(e, MethodExpr, x.typ, m, index, indirect)

		sig := m.typ.(*Signature)
		if sig.recv == nil {
			check.error(e, InvalidDeclCycle, "illegal cycle in method declaration")
			goto Error
		}

		// The receiver type becomes the type of the first function
		// argument of the method expression's function type.
		var params []*Var
		if sig.params != nil {
			params = sig.params.vars
		}
		// Be consistent about named/unnamed parameters. This is not needed
		// for type-checking, but the newly constructed signature may appear
		// in an error message and then have mixed named/unnamed parameters.
		// (An alternative would be to not print parameter names in errors,
		// but it's useful to see them; this is cheap and method expressions
		// are rare.)
		name := ""
		if len(params) > 0 && params[0].name != "" {
			// name needed
			name = sig.recv.name
			if name == "" {
				name = "_"
			}
		}
		params = append([]*Var{NewVar(sig.recv.pos, sig.recv.pkg, name, x.typ)}, params...)
		x.mode = value
		x.typ = &Signature{
			tparams:  sig.tparams,
			params:   NewTuple(params...),
			results:  sig.results,
			variadic: sig.variadic,
		}

		check.addDeclDep(m)

	} else {
		// regular selector
		switch obj := obj.(type) {
		case *Var:
			check.recordSelection(e, FieldVal, x.typ, obj, index, indirect)
			if x.mode == variable || indirect {
				x.mode = variable
			} else {
				x.mode = value
			}
			x.typ = obj.typ

		case *Func:
			// TODO(gri) If we needed to take into account the receiver's
			// addressability, should we report the type &(x.typ) instead?
			check.recordSelection(e, MethodVal, x.typ, obj, index, indirect)

			x.mode = value

			// remove receiver
			sig := *obj.typ.(*Signature)
			sig.recv = nil
			x.typ = &sig

			check.addDeclDep(obj)

		default:
			unreachable()
		}
	}

	// everything went well
	x.expr = e
	return

Error:
	x.mode = invalid
	x.expr = e
}

// use type-checks each argument.
// Useful to make sure expressions are evaluated
// (and variables are "used") in the presence of
// other errors. Arguments may be nil.
// Reports if all arguments evaluated without error.
func (check *Checker) use(args ...syntax.Expr) bool { return check.useN(args, false) }

// useLHS is like use, but doesn't "use" top-level identifiers.
// It should be called instead of use if the arguments are
// expressions on the lhs of an assignment.
func (check *Checker) useLHS(args ...syntax.Expr) bool { return check.useN(args, true) }

func (check *Checker) useN(args []syntax.Expr, lhs bool) bool {
	ok := true
	for _, e := range args {
		if !check.use1(e, lhs) {
			ok = false
		}
	}
	return ok
}

func (check *Checker) use1(e syntax.Expr, lhs bool) bool {
	var x operand
	x.mode = value // anything but invalid
	switch n := unparen(e).(type) {
	case nil:
		// nothing to do
	case *syntax.Name:
		// don't report an error evaluating blank
		if n.Value == "_" {
			break
		}
		// If the lhs is an identifier denoting a variable v, this assignment
		// is not a 'use' of v. Remember current value of v.used and restore
		// after evaluating the lhs via check.rawExpr.
		var v *Var
		var v_used bool
		if lhs {
			if _, obj := check.scope.LookupParent(n.Value, nopos); obj != nil {
				// It's ok to mark non-local variables, but ignore variables
				// from other packages to avoid potential race conditions with
				// dot-imported variables.
				if w, _ := obj.(*Var); w != nil && w.pkg == check.pkg {
					v = w
					v_used = v.used
				}
			}
		}
		check.exprOrType(&x, n, true)
		if v != nil {
			v.used = v_used // restore v.used
		}
	case *syntax.ListExpr:
		return check.useN(n.ElemList, lhs)
	default:
		check.rawExpr(nil, &x, e, nil, true)
	}
	return x.mode != invalid
}
