package gno

// XXX rename file to machine.go.

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gnolang/gno/pkgs/errors"
	"github.com/gnolang/gno/pkgs/std"
)

//----------------------------------------
// Machine

type Machine struct {

	// State
	Ops       []Op // main operations
	NumOps    int
	Values    []TypedValue  // buffer of values to be operated on
	NumValues int           // number of values
	Exprs     []Expr        // pending expressions
	Stmts     []Stmt        // pending statements
	Blocks    []*Block      // block (scope) stack
	Frames    []Frame       // func call stack
	Package   *PackageValue // active package
	Realm     *Realm        // active realm
	Exception *TypedValue   // if panic'd unless recovered

	// Volatile State
	NumResults int // number of results returned

	// Configuration
	CheckTypes bool // not yet used
	ReadOnly   bool

	Output  io.Writer
	Store   Store
	Context interface{}
}

// Machine with new package of given path.
// Creates a new MemRealmer for any new realms.
// Looks in store for package of pkgPath; if not found,
// creates new instances as necessary.
// If pkgPath is zero, the machine has no active package
// and one must be set prior to usage.
func NewMachine(pkgPath string, store Store) *Machine {
	return NewMachineWithOptions(
		MachineOptions{
			PkgPath: pkgPath,
			Store:   store,
		})
}

type MachineOptions struct {
	PkgPath    string
	CheckTypes bool // not yet used
	ReadOnly   bool
	Output     io.Writer
	Store      Store
	Context    interface{}
}

func NewMachineWithOptions(opts MachineOptions) *Machine {
	checkTypes := opts.CheckTypes
	readOnly := opts.ReadOnly
	output := opts.Output
	if output == nil {
		output = os.Stdout
	}
	store := opts.Store
	if store == nil {
		// bare store, no stdlibs.
		store = NewStore(nil, nil)
	}
	pv := (*PackageValue)(nil)
	if opts.PkgPath != "" {
		pv = store.GetPackage(opts.PkgPath, false)
		if pv == nil {
			pkgName := defaultPkgName(opts.PkgPath)
			pn := NewPackageNode(pkgName, opts.PkgPath, &FileSet{})
			pv = pn.NewPackage()
			store.SetBlockNode(pn)
			store.SetCachePackage(pv)
		}
	}
	context := opts.Context
	mm := &Machine{
		Ops:        make([]Op, 1024),
		NumOps:     0,
		Values:     make([]TypedValue, 1024),
		NumValues:  0,
		Package:    pv,
		CheckTypes: checkTypes,
		ReadOnly:   readOnly,
		Output:     output,
		Store:      store,
		Context:    context,
	}
	if pv != nil {
		mm.SetActivePackage(pv)
	}
	return mm
}

func (m *Machine) SetActivePackage(pv *PackageValue) {
	if err := m.CheckEmpty(); err != nil {
		panic(errors.Wrap(err, "set package when machine not empty"))
	}
	m.Package = pv
	m.Realm = pv.GetRealm()
	m.Blocks = []*Block{
		pv.GetBlock(m.Store),
	}
}

//----------------------------------------
// top level Run* methods.

// Upon restart, preprocess all MemPackage and save blocknodes.
// This is a temporary measure until we optimize/make-lazy.
func (m *Machine) PreprocessAllFilesAndSaveBlockNodes() {
	ch := m.Store.IterMemPackage()
	for memPkg := range ch {
		fset := ParseMemPackage(memPkg)
		pn := NewPackageNode(Name(memPkg.Name), memPkg.Path, fset)
		m.Store.SetBlockNode(pn)
		PredefineFileSet(m.Store, pn, fset)
		for _, fn := range fset.Files {
			// Save Types to m.Store (while preprocessing).
			fn = Preprocess(m.Store, pn, fn).(*FileNode)
			// Save BlockNodes to m.Store.
			SaveBlockNodes(m.Store, fn)
		}
		// Normally, the fileset would be added onto the
		// package node only after runFiles(), but we cannot
		// run files upon restart (only preprocess them).
		// So, add them here instead.
		// TODO: is this right?
		if pn.FileSet == nil {
			pn.FileSet = fset
		} else {
			// This happens for non-realm file tests.
			// TODO ensure the files are the same.
		}

	}
}

//----------------------------------------
// top level Run* methods.

// Parses files, sets the package if doesn't exist, runs files, saves mempkg
// and corresponding package node, package value, and types to store. Save
// is set to false for tests where package values may be native.
func (m *Machine) RunMemPackage(memPkg *std.MemPackage, save bool) (*PackageNode, *PackageValue) {
	// parse files.
	files := ParseMemPackage(memPkg)
	// make and set package if doesn't exist.
	pn := (*PackageNode)(nil)
	pv := (*PackageValue)(nil)
	if m.Package != nil && m.Package.PkgPath == memPkg.Path {
		pv = m.Package
		loc := PackageNodeLocation(memPkg.Path)
		pn = m.Store.GetBlockNode(loc).(*PackageNode)
	} else {
		pn = NewPackageNode(Name(memPkg.Name), memPkg.Path, &FileSet{})
		pv = pn.NewPackage()
		m.Store.SetBlockNode(pn)
		m.Store.SetCachePackage(pv)
	}
	m.SetActivePackage(pv)
	// run files.
	m.RunFiles(files.Files...)
	// maybe save package value and mempackage.
	if save {
		// store package values and types
		m.savePackageValuesAndTypes()
		// store mempackage
		m.Store.AddMemPackage(memPkg)
	}
	return pn, pv
}

// Tests all test files in a mempackage.
// Assumes that the importing of packages is handled elsewhere.
// The resulting package value and node become injected with TestMethods and
// other declarations, so it is expected that non-test code will not be run
// afterwards from the same store.
func (m *Machine) TestMemPackage(t *testing.T, memPkg *std.MemPackage) {
	defer m.injectLocOnPanic()
	DisableDebug()
	fmt.Println("DEBUG DISABLED (FOR TEST DEPENDENCIES INIT)")
	// prefetch the testing package.
	testingpv := m.Store.GetPackage("testing", false)
	testingtv := TypedValue{T: gPackageType, V: testingpv}
	testingcx := &ConstExpr{TypedValue: testingtv}
	// parse test files.
	tfiles, itfiles := ParseMemPackageTests(memPkg)
	{ // first, tfiles which run in the same package.
		pv := m.Store.GetPackage(memPkg.Path, false)
		pvBlock := pv.GetBlock(m.Store)
		pvSize := len(pvBlock.Values)
		m.SetActivePackage(pv)
		// run test files.
		m.RunFiles(tfiles.Files...)
		// run all tests in test files.
		for i := pvSize; i < len(pvBlock.Values); i++ {
			tv := &pvBlock.Values[i]
			if !(tv.T.Kind() == FuncKind &&
				strings.HasPrefix(
					string(tv.V.(*FuncValue).Name),
					"Test")) {
				continue // not a test function.
			}
			// XXX ensure correct func type.
			name := tv.V.(*FuncValue).Name
			t.Run(string(name), func(t *testing.T) {
				defer m.injectLocOnPanic()
				x := Call(name, Call(Sel(testingcx, "NewT"), Str(string(name))))
				res := m.Eval(x)
				if len(res) != 0 {
					panic(fmt.Sprintf(
						"expected no results but got %d",
						len(res)))
				}
			})
		}
	}
	{ // run all (import) tests in test files.
		pn := NewPackageNode(Name(memPkg.Name+"_test"), memPkg.Path+"_test", itfiles)
		pv := pn.NewPackage()
		m.Store.SetBlockNode(pn)
		m.Store.SetCachePackage(pv)
		pvBlock := pv.GetBlock(m.Store)
		m.SetActivePackage(pv)
		m.RunFiles(itfiles.Files...)
		pn.PrepareNewValues(pv)
		EnableDebug()
		fmt.Println("DEBUG ENABLED")
		for i := 0; i < len(pvBlock.Values); i++ {
			tv := &pvBlock.Values[i]
			if !(tv.T.Kind() == FuncKind &&
				strings.HasPrefix(
					string(tv.V.(*FuncValue).Name),
					"Test")) {
				continue // not a test function.
			}
			// XXX ensure correct func type.
			name := tv.V.(*FuncValue).Name
			t.Run(string(name), func(t *testing.T) {
				defer m.injectLocOnPanic()
				x := Call(name, Call(Sel(testingcx, "NewT"), Str(string(name))))
				res := m.Eval(x)
				if len(res) != 0 {
					fmt.Println("ERROR")
					panic(fmt.Sprintf(
						"expected no results but got %d",
						len(res)))
				}
			})
		}
	}
}

// in case of panic, inject location information to exception.
func (m *Machine) injectLocOnPanic() {
	if r := recover(); r != nil {
		// Show last location information.
		// First, determine the line number of expression or statement if any.
		lastLine := 0
		if len(m.Exprs) > 0 {
			for i := len(m.Exprs) - 1; i >= 0; i-- {
				expr := m.Exprs[i]
				if expr.GetLine() > 0 {
					lastLine = expr.GetLine()
					break
				}
			}
		}
		if lastLine == 0 && len(m.Stmts) > 0 {
			for i := len(m.Stmts) - 1; i >= 0; i-- {
				stmt := m.Stmts[i]
				if stmt.GetLine() > 0 {
					lastLine = stmt.GetLine()
					break
				}
			}
		}
		// Append line number to block location.
		lastLoc := Location{}
		for i := len(m.Blocks) - 1; i >= 0; i-- {
			block := m.Blocks[i]
			src := block.GetSource(m.Store)
			loc := src.GetLocation()
			if !loc.IsZero() {
				lastLoc = loc
				if lastLine > 0 {
					lastLoc.Line = lastLine
				}
				break
			}
		}
		// wrap panic with location information.
		if !lastLoc.IsZero() {
			fmt.Printf("%s: %v\n", lastLoc.String(), r)
			panic(errors.Wrap(r, fmt.Sprintf("location: %s", lastLoc.String())))
		} else {
			panic(r)
		}
	}
}

// Add files to the package's *FileSet and run them.
// This will also run each init function encountered.
func (m *Machine) RunFiles(fns ...*FileNode) {
	m.runFiles(fns...)
}

func (m *Machine) runFiles(fns ...*FileNode) {
	// Files' package names must match the machine's active one.
	// if there is one.
	for _, fn := range fns {
		if fn.PkgName != "" && fn.PkgName != m.Package.PkgName {
			panic(fmt.Sprintf("expected package name [%s] but got [%s]",
				m.Package.PkgName, fn.PkgName))
		}
	}
	// Add files to *PackageNode.FileSet.
	pv := m.Package
	pb := pv.GetBlock(m.Store)
	pn := pb.GetSource(m.Store).(*PackageNode)
	fs := &FileSet{Files: fns}
	fdeclared := map[Name]struct{}{}
	if pn.FileSet == nil {
		pn.FileSet = fs
	} else {
		// collect pre-existing declared names
		for _, fn := range pn.FileSet.Files {
			for _, decl := range fn.Decls {
				for _, name := range decl.GetDeclNames() {
					fdeclared[name] = struct{}{}
				}
			}
		}
		// add fns to pre-existing fileset.
		pn.FileSet.AddFiles(fns...)
	}

	// Predefine declarations across all files.
	PredefineFileSet(m.Store, pn, fs)

	// Preprocess each new file.
	for _, fn := range fns {
		// Preprocess file.
		// NOTE: Most of the declaration is handled by
		// Preprocess and any constant values set on
		// pn.StaticBlock, and those values are copied to the
		// runtime package value via PrepareNewValues.  Then,
		// non-constant var declarations and file-level imports
		// are re-set in runDeclaration(,true).
		fn = Preprocess(m.Store, pn, fn).(*FileNode)
		if debug {
			debug.Printf("PREPROCESSED FILE: %v\n", fn)
		}
		// After preprocessing, save blocknodes to store.
		SaveBlockNodes(m.Store, fn)
		// Make block for fn.
		// Each file for each *PackageValue gets its own file *Block,
		// with values copied over from each file's
		// *FileNode.StaticBlock.
		fb := NewBlock(fn, pb)
		fb.Values = make([]TypedValue, len(fn.StaticBlock.Values))
		copy(fb.Values, fn.StaticBlock.Values)
		pv.AddFileBlock(fn.Name, fb)
	}

	// Get new values across all files in package.
	updates := pn.PrepareNewValues(pv)

	// to detect loops in var declarations.
	var loopfindr = []Name{}
	// recursive function for var declarations.
	var runDeclarationFor func(fn *FileNode, decl Decl)
	runDeclarationFor = func(fn *FileNode, decl Decl) {
		// get fileblock of fn.
		// fb := pv.GetFileBlock(nil, fn.Name)
		// get dependencies of decl.
		deps := make(map[Name]struct{})
		findDependentNames(decl, deps)
		for dep, _ := range deps {
			// if dep already defined as import, skip.
			if _, ok := fn.GetLocalIndex(dep); ok {
				continue
			}
			// if dep already in fdeclared, skip.
			if _, ok := fdeclared[dep]; ok {
				continue
			}
			fn, depdecl, exists := pn.FileSet.GetDeclForSafe(dep)
			// special case: if doesn't exist:
			if !exists {
				if isUverseName(dep) { // then is reserved keyword in uverse.
					continue
				} else { // is an undefined dependency.
					panic(fmt.Sprintf(
						"dependency %s not defined in fileset with files %v",
						dep, fs.FileNames()))
				}
			}
			// if dep already in loopfindr, abort.
			if hasName(dep, loopfindr) {
				if _, ok := (*depdecl).(*FuncDecl); ok {
					// recursive function dependencies
					// are OK with func decls.
					continue
				} else {
					panic(fmt.Sprintf(
						"loop in variable initialization: dependency trail %v circularly depends on %s", loopfindr, dep))
				}
			}
			// run dependecy declaration
			loopfindr = append(loopfindr, dep)
			runDeclarationFor(fn, *depdecl)
			loopfindr = loopfindr[:len(loopfindr)-1]
		}
		// run declaration
		fb := pv.GetFileBlock(m.Store, fn.Name)
		m.PushBlock(fb)
		m.runDeclaration(decl)
		m.PopBlock()
		for _, n := range decl.GetDeclNames() {
			fdeclared[n] = struct{}{}
		}
	}

	// Declarations (and variable initializations).  This must happen
	// after all files are preprocessed, because value decl may be out of
	// order and depend on other files.

	// Run declarations.
	for _, fn := range fns {
		for _, decl := range fn.Decls {
			runDeclarationFor(fn, decl)
		}
	}

	// Run new init functions.
	// Go spec: "To ensure reproducible initialization
	// behavior, build systems are encouraged to present
	// multiple files belonging to the same package in
	// lexical file name order to a compiler."
	for _, tv := range updates {
		if tv.IsDefined() && tv.T.Kind() == FuncKind && tv.V != nil {
			fv, ok := tv.V.(*FuncValue)
			if !ok {
				continue // skip native functions.
			}
			if strings.HasPrefix(string(fv.Name), "init.") {
				fb := pv.GetFileBlock(m.Store, fv.FileName)
				m.PushBlock(fb)
				m.RunFunc(fv.Name)
				m.PopBlock()
			}
		}
	}

}

// Save the machine's package using realm finalization deep crawl.
// Also saves declared types.
func (m *Machine) savePackageValuesAndTypes() {
	// save package value and dependencies.
	pv := m.Package
	if pv.IsRealm() {
		rlm := pv.Realm
		rlm.MarkNewReal(pv)
		rlm.FinalizeRealmTransaction(m.ReadOnly, m.Store)
		// save package realm info.
		m.Store.SetPackageRealm(rlm)
	} else { // use a throwaway realm.
		rlm := NewRealm(pv.PkgPath)
		rlm.MarkNewReal(pv)
		rlm.FinalizeRealmTransaction(m.ReadOnly, m.Store)
	}
	// save declared types.
	if bv, ok := pv.Block.(*Block); ok {
		for _, tv := range bv.Values {
			if tvv, ok := tv.V.(TypeValue); ok {
				if dt, ok := tvv.Type.(*DeclaredType); ok {
					m.Store.SetType(dt)
				}
			}
		}
	}
}

func (m *Machine) RunFunc(fn Name) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Machine.RunFunc(%q) panic: %v\n%s\n",
				fn, r, m.String())
			panic(r)
		}
	}()
	m.RunStatement(S(Call(Nx(fn))))
}

func (m *Machine) RunMain() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Machine.RunMain() panic: %v\n%s\n",
				r, m.String())
			panic(r)
		}
	}()
	m.RunStatement(S(Call(X("main"))))
}

// Evaluate throwaway expression in new block scope.
// If x is a function call, it may return any number of
// results including 0.  Otherwise it returns 1.
// Input must not have been preprocessed, that is,
// it should not be the child of any parent.
func (m *Machine) Eval(x Expr) []TypedValue {
	if debug {
		m.Printf("Machine.Eval(%v)\n", x)
	}
	// X must not have been preprocessed.
	if x.GetAttribute(ATTR_PREPROCESSED) != nil {
		panic(fmt.Sprintf(
			"Machine.Eval(x) expression already preprocessed: %s",
			x.String()))
	}
	// Preprocess input using last block context.
	last := m.LastBlock().GetSource(m.Store)
	// Transform expression to ensure isolation.
	// This is to ensure that the parent context
	// doesn't get modified.
	// XXX Just use a BlockStmt?
	if _, ok := x.(*CallExpr); !ok {
		x = Call(Fn(nil, Flds("x", InterfaceT(nil)),
			Ss(
				Return(x),
			)))
	} else {
		// x already creates its own scope.
	}
	// Preprocess x.
	x = Preprocess(m.Store, last, x).(Expr)
	// Evaluate x.
	start := m.NumValues
	m.PushOp(OpHalt)
	m.PushExpr(x)
	m.PushOp(OpEval)
	m.Run()
	res := m.ReapValues(start)
	return res
}

// Evaluate any preprocessed expression statically.
// This is primiarily used by the preprocessor to evaluate
// static types and values.
func (m *Machine) EvalStatic(last BlockNode, x Expr) TypedValue {
	if debug {
		m.Printf("Machine.EvalStatic(%v, %v)\n", last, x)
	}
	// X must have been preprocessed.
	if x.GetAttribute(ATTR_PREPROCESSED) == nil {
		panic(fmt.Sprintf(
			"Machine.EvalStatic(x) expression not yet preprocessed: %s",
			x.String()))
	}
	// Temporarily push last to m.Blocks.
	m.PushBlock(last.GetStaticBlock().GetBlock())
	// Evaluate x.
	start := m.NumValues
	m.PushOp(OpHalt)
	m.PushOp(OpPopBlock)
	m.PushExpr(x)
	m.PushOp(OpEval)
	m.Run()
	res := m.ReapValues(start)
	if len(res) != 1 {
		panic("should not happen")
	}
	return res[0]
}

// Evaluate the type of any preprocessed expression statically.
// This is primiarily used by the preprocessor to evaluate
// static types of nodes.
func (m *Machine) EvalStaticTypeOf(last BlockNode, x Expr) Type {
	if debug {
		m.Printf("Machine.EvalStaticTypeOf(%v, %v)\n", last, x)
	}
	// X must have been preprocessed.
	if x.GetAttribute(ATTR_PREPROCESSED) == nil {
		panic(fmt.Sprintf(
			"Machine.EvalStaticTypeOf(x) expression not yet preprocessed: %s",
			x.String()))
	}
	// Temporarily push last to m.Blocks.
	m.PushBlock(last.GetStaticBlock().GetBlock())
	// Evaluate x.
	start := m.NumValues
	m.PushOp(OpHalt)
	m.PushOp(OpPopBlock)
	m.PushExpr(x)
	m.PushOp(OpStaticTypeOf)
	m.Run()
	res := m.ReapValues(start)
	if len(res) != 1 {
		panic("should not happen")
	}
	tv := res[0].V.(TypeValue)
	return tv.Type
}

func (m *Machine) RunStatement(s Stmt) {
	sn := m.LastBlock().GetSource(m.Store)
	s = Preprocess(m.Store, sn, s).(Stmt)
	m.PushOp(OpHalt)
	m.PushStmt(s)
	m.PushOp(OpExec)
	m.Run()
}

// Runs a declaration after preprocessing d.  If d was already
// preprocessed, call runDeclaration() instead.
// This function is primarily for testing, so no blocknodes are
// saved to store, and declarations are not realm compatible.
// NOTE: to support realm persistence of types, must
// first require the validation of blocknode locations.
func (m *Machine) RunDeclaration(d Decl) {
	// Preprocess input using package block.  There should only
	// be one block right now, and it's a *PackageNode.
	pn := m.LastBlock().GetSource(m.Store).(*PackageNode)
	d = Preprocess(m.Store, pn, d).(Decl)
	// do not SaveBlockNodes(m.Store, d).
	pn.PrepareNewValues(m.Package)
	m.runDeclaration(d)
	if debug {
		if pn != m.Package.GetBlock(m.Store).GetSource(m.Store) {
			panic("package mismatch")
		}
	}
}

// Declarations to be run within a body (not at the file or
// package level, for which evaluations happen during
// preprocessing).
func (m *Machine) runDeclaration(d Decl) {
	switch d := d.(type) {
	case *FuncDecl:
		// nothing to do.
		// closure and package already set
		// during PackageNode.NewPackage().
	case *ValueDecl:
		m.PushOp(OpHalt)
		m.PushStmt(d)
		m.PushOp(OpExec)
		m.Run()
	case *TypeDecl:
		m.PushOp(OpHalt)
		m.PushStmt(d)
		m.PushOp(OpExec)
		m.Run()
	default:
		// Do nothing for package constants.
	}
}

//----------------------------------------
// Op

type Op uint8

const (

	/* Control operators */
	OpInvalid             Op = 0x00 // invalid
	OpHalt                Op = 0x01 // halt (e.g. last statement)
	OpNoop                Op = 0x02 // no-op
	OpExec                Op = 0x03 // exec next statement
	OpPrecall             Op = 0x04 // sets X (func) to frame
	OpCall                Op = 0x05 // call(Frame.Func, [...])
	OpCallNativeBody      Op = 0x06 // call body is native
	OpReturn              Op = 0x07 // return ...
	OpReturnFromBlock     Op = 0x08 // return results (after defers)
	OpReturnToBlock       Op = 0x09 // copy results to block (before defer)
	OpDefer               Op = 0x0A // defer call(X, [...])
	OpCallDeferNativeBody Op = 0x0B // call body is native
	OpGo                  Op = 0x0C // go call(X, [...])
	OpSelect              Op = 0x0D // exec next select case
	OpSwitchClause        Op = 0x0E // exec next switch clause
	OpSwitchClauseCase    Op = 0x0F // exec next switch clause case
	OpTypeSwitch          Op = 0x10 // exec type switch clauses (all)
	OpIfCond              Op = 0x11 // eval cond
	OpPopValue            Op = 0x12 // pop X
	OpPopResults          Op = 0x13 // pop n call results
	OpPopBlock            Op = 0x14 // pop block NOTE breaks certain invariants.
	OpPopFrameAndReset    Op = 0x15 // pop frame and reset.
	OpPanic1              Op = 0x16 // pop exception and pop call frames.
	OpPanic2              Op = 0x17 // pop call frames.

	/* Unary & binary operators */
	OpUpos  Op = 0x20 // + (unary)
	OpUneg  Op = 0x21 // - (unary)
	OpUnot  Op = 0x22 // ! (unary)
	OpUxor  Op = 0x23 // ^ (unary)
	OpUrecv Op = 0x25 // <- (unary) // TODO make expr
	OpLor   Op = 0x26 // ||
	OpLand  Op = 0x27 // &&
	OpEql   Op = 0x28 // ==
	OpNeq   Op = 0x29 // !=
	OpLss   Op = 0x2A // <
	OpLeq   Op = 0x2B // <=
	OpGtr   Op = 0x2C // >
	OpGeq   Op = 0x2D // >=
	OpAdd   Op = 0x2E // +
	OpSub   Op = 0x2F // -
	OpBor   Op = 0x30 // |
	OpXor   Op = 0x31 // ^
	OpMul   Op = 0x32 // *
	OpQuo   Op = 0x33 // /
	OpRem   Op = 0x34 // %
	OpShl   Op = 0x35 // <<
	OpShr   Op = 0x36 // >>
	OpBand  Op = 0x37 // &
	OpBandn Op = 0x38 // &^

	/* Other expression operators */
	OpEval         Op = 0x40 // eval next expression
	OpBinary1      Op = 0x41 // X op ?
	OpIndex1       Op = 0x42 // X[Y]
	OpIndex2       Op = 0x43 // (_, ok :=) X[Y]
	OpSelector     Op = 0x44 // X.Y
	OpSlice        Op = 0x45 // X[Low:High:Max]
	OpStar         Op = 0x46 // *X (deref or pointer-to)
	OpRef          Op = 0x47 // &X
	OpTypeAssert1  Op = 0x48 // X.(Type)
	OpTypeAssert2  Op = 0x49 // (_, ok :=) X.(Type)
	OpStaticTypeOf Op = 0x4A // static type of X
	OpCompositeLit Op = 0x4B // X{???}
	OpArrayLit     Op = 0x4C // [Len]{...}
	OpSliceLit     Op = 0x4D // []{value,...}
	OpSliceLit2    Op = 0x4E // []{key:value,...}
	OpMapLit       Op = 0x4F // X{...}
	OpStructLit    Op = 0x50 // X{...}
	OpFuncLit      Op = 0x51 // func(T){Body}
	OpConvert      Op = 0x52 // Y(X)

	/* Native operators */
	OpArrayLitGoNative  Op = 0x60
	OpSliceLitGoNative  Op = 0x61
	OpStructLitGoNative Op = 0x62
	OpCallGoNative      Op = 0x63

	/* Type operators */
	OpFieldType       Op = 0x70 // Name: X `tag`
	OpArrayType       Op = 0x71 // [X]Y{}
	OpSliceType       Op = 0x72 // []X{}
	OpPointerType     Op = 0x73 // *X
	OpInterfaceType   Op = 0x74 // interface{...}
	OpChanType        Op = 0x75 // [<-]chan[<-]X
	OpFuncType        Op = 0x76 // func(params...)results...
	OpMapType         Op = 0x77 // map[X]Y
	OpStructType      Op = 0x78 // struct{...}
	OpMaybeNativeType Op = 0x79 // maybenative{X}

	/* Statement operators */
	OpAssign      Op = 0x80 // Lhs = Rhs
	OpAddAssign   Op = 0x81 // Lhs += Rhs
	OpSubAssign   Op = 0x82 // Lhs -= Rhs
	OpMulAssign   Op = 0x83 // Lhs *= Rhs
	OpQuoAssign   Op = 0x84 // Lhs /= Rhs
	OpRemAssign   Op = 0x85 // Lhs %= Rhs
	OpBandAssign  Op = 0x86 // Lhs &= Rhs
	OpBandnAssign Op = 0x87 // Lhs &^= Rhs
	OpBorAssign   Op = 0x88 // Lhs |= Rhs
	OpXorAssign   Op = 0x89 // Lhs ^= Rhs
	OpShlAssign   Op = 0x8A // Lhs <<= Rhs
	OpShrAssign   Op = 0x8B // Lhs >>= Rhs
	OpDefine      Op = 0x8C // X... := Y...
	OpInc         Op = 0x8D // X++
	OpDec         Op = 0x8E // X--

	/* Decl operators */
	OpValueDecl Op = 0x90 // var/const ...
	OpTypeDecl  Op = 0x91 // type ...

	/* Loop (sticky) operators (>= 0xD0) */
	OpSticky            Op = 0xD0 // not a real op.
	OpBody              Op = 0xD1 // if/block/switch/select.
	OpForLoop           Op = 0xD2
	OpRangeIter         Op = 0xD3
	OpRangeIterString   Op = 0xD4
	OpRangeIterMap      Op = 0xD5
	OpRangeIterArrayPtr Op = 0xD6
	OpReturnCallDefers  Op = 0xD7 // TODO rename?
)

//----------------------------------------
// main run loop.

func (m *Machine) Run() {
	for {
		op := m.PopOp()
		// TODO: this can be optimized manually, even into tiers.
		switch op {
		/* Control operators */
		case OpHalt:
			return
		case OpNoop:
			continue
		case OpExec:
			m.doOpExec(op)
		case OpPrecall:
			m.doOpPrecall()
		case OpCall:
			m.doOpCall()
		case OpCallNativeBody:
			m.doOpCallNativeBody()
		case OpReturn:
			m.doOpReturn()
		case OpReturnFromBlock:
			m.doOpReturnFromBlock()
		case OpReturnToBlock:
			m.doOpReturnToBlock()
		case OpDefer:
			m.doOpDefer()
		case OpPanic1:
			m.doOpPanic1()
		case OpPanic2:
			m.doOpPanic2()
		case OpCallDeferNativeBody:
			m.doOpCallDeferNativeBody()
		case OpGo:
			panic("not yet implemented")
		case OpSelect:
			panic("not yet implemented")
		case OpSwitchClause:
			m.doOpSwitchClause()
		case OpSwitchClauseCase:
			m.doOpSwitchClauseCase()
		case OpTypeSwitch:
			m.doOpTypeSwitch()
		case OpIfCond:
			m.doOpIfCond()
		case OpPopValue:
			m.PopValue()
		case OpPopResults:
			m.PopResults()
		case OpPopBlock:
			m.PopBlock()
		case OpPopFrameAndReset:
			m.PopFrameAndReset()
		/* Unary operators */
		case OpUpos:
			m.doOpUpos()
		case OpUneg:
			m.doOpUneg()
		case OpUnot:
			m.doOpUnot()
		case OpUxor:
			m.doOpUxor()
		case OpUrecv:
			m.doOpUrecv()
		/* Binary operators */
		case OpLor:
			m.doOpLor()
		case OpLand:
			m.doOpLand()
		case OpEql:
			m.doOpEql()
		case OpNeq:
			m.doOpNeq()
		case OpLss:
			m.doOpLss()
		case OpLeq:
			m.doOpLeq()
		case OpGtr:
			m.doOpGtr()
		case OpGeq:
			m.doOpGeq()
		case OpAdd:
			m.doOpAdd()
		case OpSub:
			m.doOpSub()
		case OpBor:
			m.doOpBor()
		case OpXor:
			m.doOpXor()
		case OpMul:
			m.doOpMul()
		case OpQuo:
			m.doOpQuo()
		case OpRem:
			m.doOpRem()
		case OpShl:
			m.doOpShl()
		case OpShr:
			m.doOpShr()
		case OpBand:
			m.doOpBand()
		case OpBandn:
			m.doOpBandn()
		/* Expression operators */
		case OpEval:
			m.doOpEval()
		case OpBinary1:
			m.doOpBinary1()
		case OpIndex1:
			m.doOpIndex1()
		case OpIndex2:
			m.doOpIndex2()
		case OpSelector:
			m.doOpSelector()
		case OpSlice:
			m.doOpSlice()
		case OpStar:
			m.doOpStar()
		case OpRef:
			m.doOpRef()
		case OpTypeAssert1:
			m.doOpTypeAssert1()
		case OpTypeAssert2:
			m.doOpTypeAssert2()
		case OpStaticTypeOf:
			m.doOpStaticTypeOf()
		case OpCompositeLit:
			m.doOpCompositeLit()
		case OpArrayLit:
			m.doOpArrayLit()
		case OpSliceLit:
			m.doOpSliceLit()
		case OpSliceLit2:
			m.doOpSliceLit2()
		case OpFuncLit:
			m.doOpFuncLit()
		case OpMapLit:
			m.doOpMapLit()
		case OpStructLit:
			m.doOpStructLit()
		case OpConvert:
			m.doOpConvert()
		/* GoNative Operators */
		case OpArrayLitGoNative:
			m.doOpArrayLitGoNative()
		case OpSliceLitGoNative:
			m.doOpSliceLitGoNative()
		case OpStructLitGoNative:
			m.doOpStructLitGoNative()
		case OpCallGoNative:
			m.doOpCallGoNative()
		/* Type operators */
		case OpFieldType:
			m.doOpFieldType()
		case OpArrayType:
			m.doOpArrayType()
		case OpSliceType:
			m.doOpSliceType()
		case OpChanType:
			m.doOpChanType()
		case OpFuncType:
			m.doOpFuncType()
		case OpMapType:
			m.doOpMapType()
		case OpStructType:
			m.doOpStructType()
		case OpInterfaceType:
			m.doOpInterfaceType()
		case OpMaybeNativeType:
			m.doOpMaybeNativeType()
		/* Statement operators */
		case OpAssign:
			m.doOpAssign()
		case OpAddAssign:
			m.doOpAddAssign()
		case OpSubAssign:
			m.doOpSubAssign()
		case OpMulAssign:
			m.doOpMulAssign()
		case OpQuoAssign:
			m.doOpQuoAssign()
		case OpRemAssign:
			m.doOpRemAssign()
		case OpBandAssign:
			m.doOpBandAssign()
		case OpBandnAssign:
			m.doOpBandnAssign()
		case OpBorAssign:
			m.doOpBorAssign()
		case OpXorAssign:
			m.doOpXorAssign()
		case OpShlAssign:
			m.doOpShlAssign()
		case OpShrAssign:
			m.doOpShrAssign()
		case OpDefine:
			m.doOpDefine()
		case OpInc:
			m.doOpInc()
		case OpDec:
			m.doOpDec()
		/* Decl operators */
		case OpValueDecl:
			m.doOpValueDecl()
		case OpTypeDecl:
			m.doOpTypeDecl()
		/* Loop (sticky) operators */
		case OpBody:
			m.doOpExec(op)
		case OpForLoop:
			m.doOpExec(op)
		case OpRangeIter, OpRangeIterArrayPtr:
			m.doOpExec(op)
		case OpRangeIterString:
			m.doOpExec(op)
		case OpRangeIterMap:
			m.doOpExec(op)
		case OpReturnCallDefers:
			m.doOpReturnCallDefers()
		default:
			panic(fmt.Sprintf("unexpected opcode %s", op.String()))
		}
	}
}

//----------------------------------------
// push pop methods.

func (m *Machine) PushOp(op Op) {
	if debug {
		m.Printf("+o %v\n", op)
	}
	if len(m.Ops) == m.NumOps {
		// TODO tune. also see PushValue().
		newOps := make([]Op, len(m.Ops)*2)
		copy(newOps, m.Ops)
		m.Ops = newOps
	}
	m.Ops[m.NumOps] = op
	m.NumOps++
}

func (m *Machine) PopOp() Op {
	numOps := m.NumOps
	op := m.Ops[numOps-1]
	if debug {
		m.Printf("-o %v\n", op)
	}
	if OpSticky <= op {
		// do not pop persistent op types.
	} else {
		m.NumOps--
	}
	return op
}

func (m *Machine) ForcePopOp() {
	if debug {
		m.Printf("-o! %v\n", m.Ops[m.NumOps-1])
	}
	m.NumOps--
}

// Offset starts at 1.
// DEPRECATED use PeekStmt1() instead.
func (m *Machine) PeekStmt(offset int) Stmt {
	if debug {
		if offset != 1 {
			panic("should not happen")
		}
	}
	return m.Stmts[len(m.Stmts)-offset]
}

func (m *Machine) PeekStmt1() Stmt {
	numStmts := len(m.Stmts)
	s := m.Stmts[numStmts-1]
	if bs, ok := s.(*bodyStmt); ok {
		return bs.Active
	} else {
		return m.Stmts[numStmts-1]
	}
}

func (m *Machine) PushStmt(s Stmt) {
	if debug {
		m.Printf("+s %v\n", s)
	}
	m.Stmts = append(m.Stmts, s)
}

func (m *Machine) PushStmts(ss ...Stmt) {
	if debug {
		for _, s := range ss {
			m.Printf("+s %v\n", s)
		}
	}
	m.Stmts = append(m.Stmts, ss...)
}

func (m *Machine) PopStmt() Stmt {
	numStmts := len(m.Stmts)
	s := m.Stmts[numStmts-1]
	if debug {
		m.Printf("-s %v\n", s)
	}
	if bs, ok := s.(*bodyStmt); ok {
		return bs.PopActiveStmt()
	} else {
		// general case.
		m.Stmts = m.Stmts[:numStmts-1]
		return s
	}
}

func (m *Machine) ForcePopStmt() (s Stmt) {
	numStmts := len(m.Stmts)
	s = m.Stmts[numStmts-1]
	if debug {
		m.Printf("-s %v\n", s)
	}
	// TODO debug lines and assertions.
	m.Stmts = m.Stmts[:len(m.Stmts)-1]
	return
}

// Offset starts at 1.
func (m *Machine) PeekExpr(offset int) Expr {
	return m.Exprs[len(m.Exprs)-offset]
}

func (m *Machine) PushExpr(x Expr) {
	if debug {
		m.Printf("+x %v\n", x)
	}
	m.Exprs = append(m.Exprs, x)
}

func (m *Machine) PopExpr() Expr {
	numExprs := len(m.Exprs)
	x := m.Exprs[numExprs-1]
	if debug {
		m.Printf("-x %v\n", x)
	}
	m.Exprs = m.Exprs[:numExprs-1]
	return x
}

// Returns reference to value in Values stack.  Offset starts at 1.
func (m *Machine) PeekValue(offset int) *TypedValue {
	return &m.Values[m.NumValues-offset]
}

// XXX delete?
func (m *Machine) PeekType(offset int) Type {
	return m.Values[m.NumValues-offset].T
}

func (m *Machine) PushValue(tv TypedValue) {
	if debug {
		m.Printf("+v %v\n", tv)
	}
	if len(m.Values) == m.NumValues {
		// TODO tune. also see PushOp().
		newValues := make([]TypedValue, len(m.Values)*2)
		copy(newValues, m.Values)
		m.Values = newValues
	}
	m.Values[m.NumValues] = tv
	m.NumValues++
	return
}

// Resulting reference is volatile.
func (m *Machine) PopValue() (tv *TypedValue) {
	tv = &m.Values[m.NumValues-1]
	if debug {
		m.Printf("-v %v\n", tv)
	}
	m.NumValues--
	return tv
}

// Returns a slice of n values in the stack and decrements NumValues.
// NOTE: The results are on the values stack, so they must be copied or used
// immediately.  If you need to use the machine before or during usage,
// consider using PopCopyValues().
// NOTE: the values are in stack order, oldest first, the opposite order of
// multiple pop calls.  This is used for params assignment, for example.
func (m *Machine) PopValues(n int) []TypedValue {
	if debug {
		for i := 0; i < n; i++ {
			tv := m.Values[m.NumValues-n+i]
			m.Printf("-vs[%d/%d] %v\n", i, n, tv)
		}
	}
	m.NumValues -= n
	return m.Values[m.NumValues : m.NumValues+n]
}

// Like PopValues(), but copies the values onto a new slice.
func (m *Machine) PopCopyValues(n int) []TypedValue {
	res := make([]TypedValue, n)
	ptvs := m.PopValues(n)
	copy(res, ptvs)
	return res
}

// Decrements NumValues by number of last results.
func (m *Machine) PopResults() {
	if debug {
		for i := 0; i < m.NumResults; i++ {
			m.PopValue()
		}
	} else {
		m.NumValues -= m.NumResults
	}
	m.NumResults = 0
}

// Pops values with index start or greater.
func (m *Machine) ReapValues(start int) []TypedValue {
	end := m.NumValues
	rs := make([]TypedValue, end-start)
	copy(rs, m.Values[start:end])
	m.NumValues = start
	return rs
}

func (m *Machine) PushBlock(b *Block) {
	if debug {
		m.Println("+B")
	}
	m.Blocks = append(m.Blocks, b)
}

func (m *Machine) PopBlock() (b *Block) {
	if debug {
		m.Println("-B")
	}
	numBlocks := len(m.Blocks)
	b = m.Blocks[numBlocks-1]
	m.Blocks = m.Blocks[:numBlocks-1]
	return b
}

// The result is a volatile reference in the machine's type stack.
// Mutate and forget.
func (m *Machine) LastBlock() *Block {
	return m.Blocks[len(m.Blocks)-1]
}

// Pushes a frame with one less statement.
func (m *Machine) PushFrameBasic(s Stmt) {
	label := s.GetLabel()
	fr := Frame{
		Label:     label,
		Source:    s,
		NumOps:    m.NumOps,
		NumValues: m.NumValues,
		NumExprs:  len(m.Exprs),
		NumStmts:  len(m.Stmts),
		NumBlocks: len(m.Blocks),
	}
	if debug {
		m.Printf("+F %#v\n", fr)
	}
	m.Frames = append(m.Frames, fr)
}

// TODO: track breaks/panics/returns on frame and
// ensure the counts are consistent, otherwise we mask
// bugs with frame pops.
func (m *Machine) PushFrameCall(cx *CallExpr, fv *FuncValue, recv TypedValue) {
	fr := Frame{
		Source:      cx,
		NumOps:      m.NumOps,
		NumValues:   m.NumValues - cx.NumArgs - 1,
		NumExprs:    len(m.Exprs),
		NumStmts:    len(m.Stmts),
		NumBlocks:   len(m.Blocks),
		Func:        fv,
		GoFunc:      nil,
		Receiver:    recv,
		NumArgs:     cx.NumArgs,
		IsVarg:      cx.Varg,
		Defers:      nil,
		LastPackage: m.Package,
		LastRealm:   m.Realm,
	}
	if debug {
		if m.Package == nil {
			panic("should not happen")
		}
	}
	if debug {
		m.Printf("+F %#v\n", fr)
	}
	m.Frames = append(m.Frames, fr)
	pv := fv.GetPackage(m.Store)
	if pv == nil {
		panic(fmt.Sprintf("package value missing in store: %s", fv.PkgPath))
	}
	m.Package = pv
	rlm := pv.GetRealm()
	if rlm != nil && m.Realm != rlm {
		m.Realm = rlm // enter new realm
	}
}

func (m *Machine) PushFrameGoNative(cx *CallExpr, fv *NativeValue) {
	fr := Frame{
		Source:      cx,
		NumOps:      m.NumOps,
		NumValues:   m.NumValues - cx.NumArgs - 1,
		NumExprs:    len(m.Exprs),
		NumStmts:    len(m.Stmts),
		NumBlocks:   len(m.Blocks),
		Func:        nil,
		GoFunc:      fv,
		Receiver:    TypedValue{},
		NumArgs:     cx.NumArgs,
		IsVarg:      cx.Varg,
		Defers:      nil,
		LastPackage: m.Package,
		LastRealm:   m.Realm,
	}
	if debug {
		m.Printf("+F %#v\n", fr)
	}
	m.Frames = append(m.Frames, fr)
	// keep m.Package the same.
}

func (m *Machine) PopFrame() Frame {
	numFrames := len(m.Frames)
	f := m.Frames[numFrames-1]
	if debug {
		m.Printf("-F %#v\n", f)
	}
	m.Frames = m.Frames[:numFrames-1]
	return f
}

func (m *Machine) PopFrameAndReset() {
	fr := m.PopFrame()
	m.NumOps = fr.NumOps
	m.NumValues = fr.NumValues
	m.Exprs = m.Exprs[:fr.NumExprs]
	m.Stmts = m.Stmts[:fr.NumStmts]
	m.Blocks = m.Blocks[:fr.NumBlocks]
	m.PopStmt() // may be sticky
}

// TODO: optimize by passing in last frame.
func (m *Machine) PopFrameAndReturn() {
	fr := m.PopFrame()
	if debug {
		// TODO: optimize with fr.IsCall
		if fr.Func == nil && fr.GoFunc == nil {
			panic("unexpected non-call (loop) frame")
		}
	}
	rtypes := fr.Func.GetType(m.Store).Results
	numRes := len(rtypes)
	m.NumOps = fr.NumOps
	m.NumResults = numRes
	m.Exprs = m.Exprs[:fr.NumExprs]
	m.Stmts = m.Stmts[:fr.NumStmts]
	m.Blocks = m.Blocks[:fr.NumBlocks]
	// shift and convert results to typed-nil if undefined and not iface
	// kind.  and not func result type isn't interface kind.
	resStart := m.NumValues - numRes
	for i := 0; i < numRes; i++ {
		res := m.Values[resStart+i]
		if res.IsUndefined() && rtypes[i].Type.Kind() != InterfaceKind {
			res.T = rtypes[i].Type
		}
		m.Values[fr.NumValues+i] = res
	}
	m.NumValues = fr.NumValues + numRes
	m.Package = fr.LastPackage
}

func (m *Machine) PeekFrameAndContinueFor() {
	fr := m.LastFrame()
	m.NumOps = fr.NumOps + 1
	m.NumValues = fr.NumValues
	m.Exprs = m.Exprs[:fr.NumExprs]
	m.Stmts = m.Stmts[:fr.NumStmts+1]
	m.Blocks = m.Blocks[:fr.NumBlocks+1]
	ls := m.PeekStmt(1).(*bodyStmt)
	ls.NextBodyIndex = ls.BodyLen
}

func (m *Machine) PeekFrameAndContinueRange() {
	fr := m.LastFrame()
	m.NumOps = fr.NumOps + 1
	m.NumValues = fr.NumValues + 1
	m.Exprs = m.Exprs[:fr.NumExprs]
	m.Stmts = m.Stmts[:fr.NumStmts+1]
	m.Blocks = m.Blocks[:fr.NumBlocks+1]
	ls := m.PeekStmt(1).(*bodyStmt)
	ls.NextBodyIndex = ls.BodyLen
}

func (m *Machine) NumFrames() int {
	return len(m.Frames)
}

func (m *Machine) LastFrame() *Frame {
	return &m.Frames[len(m.Frames)-1]
}

// TODO: this function and PopUntilLastCallFrame() is used in conjunction
// spanning two disjoint operations upon return. Optimize.
func (m *Machine) LastCallFrame() *Frame {
	for i := len(m.Frames) - 1; i >= 0; i-- {
		fr := &m.Frames[i]
		if fr.Func != nil || fr.GoFunc != nil {
			// TODO: optimize with fr.IsCall
			return fr
		}
	}
	panic("missing call frame")
}

// pops the last non-call (loop) frames
// and returns the last call frame (which is left on stack).
func (m *Machine) PopUntilLastCallFrame() *Frame {
	for i := len(m.Frames) - 1; i >= 0; i-- {
		fr := &m.Frames[i]
		if fr.Func != nil || fr.GoFunc != nil {
			// TODO: optimize with fr.IsCall
			m.Frames = m.Frames[:i+1]
			return fr
		}
	}
	return nil
}

func (m *Machine) PushForPointer(lx Expr) {
	switch lx := lx.(type) {
	case *NameExpr:
		// no Lhs eval needed.
	case *IndexExpr:
		// evaluate Index
		m.PushExpr(lx.Index)
		m.PushOp(OpEval)
		// evaluate X
		m.PushExpr(lx.X)
		m.PushOp(OpEval)
	case *SelectorExpr:
		// evaluate X
		m.PushExpr(lx.X)
		m.PushOp(OpEval)
	case *StarExpr:
		// evaluate X (a reference)
		m.PushExpr(lx.X)
		m.PushOp(OpEval)
	case *CompositeLitExpr: // for *RefExpr e.g. &mystruct{}
		// evaluate lx.
		m.PushExpr(lx)
		m.PushOp(OpEval)
	default:
		panic(fmt.Sprintf(
			"illegal assignment X expression type %v",
			reflect.TypeOf(lx)))
	}
}

func (m *Machine) PopAsPointer(lx Expr) PointerValue {
	switch lx := lx.(type) {
	case *NameExpr:
		lb := m.LastBlock()
		return lb.GetPointerTo(m.Store, lx.Path)
	case *IndexExpr:
		iv := m.PopValue()
		xv := m.PopValue()
		return xv.GetPointerAtIndex(m.Store, iv)
	case *SelectorExpr:
		xv := m.PopValue()
		return xv.GetPointerTo(m.Store, lx.Path)
	case *StarExpr:
		ptr := m.PopValue().V.(PointerValue)
		return ptr
	case *CompositeLitExpr: // for *RefExpr
		tv := *m.PopValue()
		return PointerValue{
			TV:   &tv, // heap alloc
			Base: nil,
		}
	default:
		panic("should not happen")
	}
}

// for testing.
func (m *Machine) CheckEmpty() error {
	found := ""
	if m.NumOps > 0 {
		found = "op"
	} else if m.NumValues > 0 {
		found = "value"
	} else if len(m.Exprs) > 0 {
		found = "expr"
	} else if len(m.Stmts) > 0 {
		found = "stmt"
	} else if len(m.Blocks) > 0 {
		for _, b := range m.Blocks {
			_, isPkg := b.GetSource(m.Store).(*PackageNode)
			if isPkg {
				// ok
			} else {
				found = "(non-package) block"
			}
		}
	} else if len(m.Frames) > 0 {
		found = "frame"
	} else if m.NumResults > 0 {
		found = ".NumResults != 0"
	}
	if found != "" {
		return fmt.Errorf("found leftover %s", found)
	} else {
		return nil
	}
}

func (m *Machine) Panic(ex TypedValue) {
	// TODO: chain exceptions if preexisting unrecovered exception.
	m.Exception = &ex
	m.PopUntilLastCallFrame()
	m.PushOp(OpPanic2)
	m.PushOp(OpReturnCallDefers)
}

//----------------------------------------
// inspection methods

func (m *Machine) Println(args ...interface{}) {
	if debug {
		if enabled {
			s := strings.Repeat("|", m.NumOps)
			debug.Println(append([]interface{}{s}, args...)...)
		}
	}
}

func (m *Machine) Printf(format string, args ...interface{}) {
	if debug {
		if enabled {
			s := strings.Repeat("|", m.NumOps)
			debug.Printf(s+" "+format, args...)
		}
	}
}

func (m *Machine) String() string {
	vs := []string{}
	for i := m.NumValues - 1; i >= 0; i-- {
		v := m.Values[i]
		vs = append(vs, fmt.Sprintf("          #%d %v", i, v))
	}
	ss := []string{}
	for i := len(m.Stmts) - 1; i >= 0; i-- {
		s := m.Stmts[i]
		ss = append(ss, fmt.Sprintf("          #%d %v", i, s))
	}
	xs := []string{}
	for i := len(m.Exprs) - 1; i >= 0; i-- {
		x := m.Exprs[i]
		xs = append(xs, fmt.Sprintf("          #%d %v", i, x))
	}
	bs := []string{}
	for b := m.LastBlock(); b != nil; {
		gen := len(bs)/3 + 1
		gens := "@" // strings.Repeat("@", gen)
		if pv, ok := b.Source.(*PackageNode); ok {
			// package blocks have too much, so just
			// print the pkgpath.
			bs = append(bs, fmt.Sprintf("          %s(%d) %s", gens, gen, pv.PkgPath))
		} else {
			bsi := b.StringIndented("            ")
			bs = append(bs, fmt.Sprintf("          %s(%d) %s", gens, gen, bsi))
			if b.Source != nil {
				sb := b.GetSource(m.Store).GetStaticBlock().GetBlock()
				bs = append(bs, fmt.Sprintf(" (s vals) %s(%d) %s", gens, gen,
					sb.StringIndented("            ")))
				sts := b.GetSource(m.Store).GetStaticBlock().Types
				bs = append(bs, fmt.Sprintf(" (s typs) %s(%d) %s", gens, gen,
					sts))
			}
		}
		// b = b.Parent.(*Block|RefValue)
		switch bp := b.Parent.(type) {
		case nil:
			b = nil
			break
		case *Block:
			b = bp
		case RefValue:
			bs = append(bs, fmt.Sprintf("            (block ref %v)", bp.ObjectID))
			b = nil
			break
		default:
			panic("should not happen")
		}
	}
	obs := []string{}
	for i := len(m.Blocks) - 2; i >= 0; i-- {
		b := m.Blocks[i]
		if _, ok := b.Source.(*PackageNode); ok {
			break // done, skip *PackageNode.
		} else {
			obs = append(obs, fmt.Sprintf("          #%d %s", i,
				b.StringIndented("            ")))
			if b.Source != nil {
				sb := b.GetSource(m.Store).GetStaticBlock().GetBlock()
				obs = append(obs, fmt.Sprintf(" (static) #%d %s", i,
					sb.StringIndented("            ")))
			}
		}
	}
	fs := []string{}
	for i := len(m.Frames) - 1; i >= 0; i-- {
		fr := m.Frames[i]
		fs = append(fs, fmt.Sprintf("          #%d %s", i, fr.String()))
	}
	return fmt.Sprintf(`Machine:
    CheckTypes: %v
	Op: %v
	Values: (len: %d)
%s
	Exprs:
%s
	Stmts:
%s
	Blocks:
%s
	Blocks (other):
%s
	Frames:
%s
	Exception:
	  %s`,
		m.CheckTypes,
		m.Ops[:m.NumOps],
		m.NumValues,
		strings.Join(vs, "\n"),
		strings.Join(xs, "\n"),
		strings.Join(ss, "\n"),
		strings.Join(bs, "\n"),
		strings.Join(obs, "\n"),
		strings.Join(fs, "\n"),
		m.Exception,
	)
}

//----------------------------------------

func hasName(n Name, ns []Name) bool {
	for _, n2 := range ns {
		if n == n2 {
			return true
		}
	}
	return false
}
