package goparser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

const parserVersion = "1"

type Request struct {
	Root       string
	Patterns   []string
	Generation uint64
	FileHashes map[string]string
}

type Result struct {
	Files                 []core.IndexedFile
	PackageImports        map[string][]string
	ReversePackageImports map[string][]string
	FilePackages          map[string]string
	Warnings              []string
}

type Parser struct{}

func New() *Parser { return &Parser{} }

type fileOutput struct {
	indexed core.IndexedFile
	units   map[string]bool
	edges   map[string]bool
}

type namedType struct {
	id, path      string
	unit          core.CodeUnit
	named         *types.Named
	interfaceType *types.Interface
	emitted       bool
}

type packageData struct {
	pkg       *packages.Package
	module    string
	filePaths map[string]bool
}

type packageIdentity struct {
	module string
	local  bool
}

type functionIdentity struct {
	id, path, qualified string
	generated           bool
}

type packageLoad struct {
	selected, all []*packages.Package
}

type catalog struct {
	packages    map[string]packageIdentity
	functions   map[*types.Func]functionIdentity
	typeObjects map[*types.TypeName]*namedType
	types       []*namedType
}

func (p *Parser) Parse(ctx context.Context, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	root, err := canonicalRoot(req.Root)
	if err != nil {
		return Result{}, err
	}
	hashes, err := validateHashes(root, req.FileHashes)
	if err != nil {
		return Result{}, err
	}
	patterns, err := validatePatterns(req.Patterns)
	if err != nil {
		return Result{}, err
	}
	loaded, warnings, err := loadPackages(ctx, root, patterns)
	if err != nil {
		return Result{}, err
	}
	identity, err := buildCatalog(ctx, root, loaded.all)
	if err != nil {
		return Result{}, err
	}
	pkgs := loaded.selected
	result := Result{
		PackageImports:        map[string][]string{},
		ReversePackageImports: map[string][]string{},
		FilePackages:          map[string]string{},
	}
	files := map[string]*fileOutput{}
	packageFiles := map[string]string{}
	packageLines := map[string]uint32{}
	analyzable := false
	data := make([]packageData, 0, len(pkgs))

	for _, pkg := range pkgs {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		for _, packageError := range pkg.Errors {
			warnings = append(warnings, cleanWarning(root, packageError.Error()))
		}
		syntax := syntaxFiles(root, pkg)
		if len(syntax) > 0 {
			analyzable = true
		}
		current := packageData{pkg: pkg, module: modulePath(pkg), filePaths: map[string]bool{}}
		for _, item := range syntax {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			if !allowedFile(item.path, hashes) || ast.IsGenerated(item.file) {
				continue
			}
			contents, readErr := os.ReadFile(item.filename)
			if readErr != nil {
				warnings = append(warnings, item.path+": "+readErr.Error())
				continue
			}
			current.filePaths[item.path] = true
			result.FilePackages[item.path] = pkg.PkgPath
			out := files[item.path]
			if out != nil {
				continue
			}
			out = newFileOutput(item.path, req, hashes)
			files[item.path] = out
			if currentPath := packageFiles[pkg.PkgPath]; currentPath == "" || item.path < currentPath {
				packageFiles[pkg.PkgPath] = item.path
				packageLines[pkg.PkgPath] = uint32(pkg.Fset.PositionFor(item.file.Package, false).Line)
			}
			fileID := stableID(current.module, pkg.PkgPath, "file", "", item.path)
			out.addUnit(codeUnit(req, hashes, item.path, fileID, filepath.Base(item.path), item.path, "file", item.file, item.file, pkg.Fset, contents))
			if err := extractDeclarations(ctx, req, hashes, current, item, contents, out, identity); err != nil {
				return Result{}, err
			}
			if err := extractImports(ctx, req, hashes, current, item, out, identity, result.PackageImports); err != nil {
				return Result{}, err
			}
		}
		data = append(data, current)
	}
	if !analyzable {
		if len(warnings) == 0 {
			return Result{}, errors.New("no analyzable Go package under project root")
		}
		return Result{}, fmt.Errorf("no analyzable Go package under project root: %s", warnings[0])
	}

	for packagePath, path := range packageFiles {
		out := files[path]
		pkg := packageForPath(pkgs, packagePath)
		id := stableID(modulePath(pkg), packagePath, "package", "", packagePath)
		position := packageLines[packagePath]
		unit := core.CodeUnit{
			ID: id, Name: pkg.Name, QualifiedName: packagePath, Kind: "package", Language: "go", Extension: ".go",
			Path: path, Source: "package " + pkg.Name, FileHash: hashes[path], StartLine: position, EndLine: position,
			Generation: req.Generation, Weight: sourceWeight(path),
		}
		out.addUnit(unit)
		for _, filePath := range packageFilePaths(result.FilePackages, packagePath) {
			file := files[filePath]
			fileID := stableID(modulePath(pkg), packagePath, "file", "", filePath)
			file.addEdge(edgeFor(req, hashes, filePath, id, fileID, "contains", 1, 1, "local"))
		}
	}

	if err := addEmbeds(ctx, req, hashes, files, identity); err != nil {
		return Result{}, err
	}
	if err := addImplements(ctx, req, hashes, files, identity); err != nil {
		return Result{}, err
	}
	for _, current := range data {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if len(current.filePaths) == 0 || current.pkg.IllTyped || current.pkg.Types == nil || current.pkg.TypesInfo == nil {
			continue
		}
		callWarnings, err := addCalls(ctx, req, hashes, root, current.pkg, files, identity)
		if err != nil {
			return Result{}, err
		}
		warnings = append(warnings, callWarnings...)
	}

	for source, imports := range result.PackageImports {
		result.PackageImports[source] = uniqueSorted(imports)
		for _, target := range result.PackageImports[source] {
			result.ReversePackageImports[target] = append(result.ReversePackageImports[target], source)
		}
	}
	for target, importers := range result.ReversePackageImports {
		result.ReversePackageImports[target] = uniqueSorted(importers)
	}
	paths := sortedFileKeys(files)
	result.Files = make([]core.IndexedFile, 0, len(paths))
	for _, path := range paths {
		out := files[path]
		sort.Slice(out.indexed.Units, func(i, j int) bool { return out.indexed.Units[i].ID < out.indexed.Units[j].ID })
		sort.Slice(out.indexed.Edges, func(i, j int) bool { return edgeKey(out.indexed.Edges[i]) < edgeKey(out.indexed.Edges[j]) })
		result.Files = append(result.Files, out.indexed)
	}
	result.Warnings = uniqueSorted(warnings)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return result, nil
}

type syntaxFile struct {
	file     *ast.File
	filename string
	path     string
}

func syntaxFiles(root string, pkg *packages.Package) []syntaxFile {
	result := []syntaxFile{}
	for _, file := range pkg.Syntax {
		filename := pkg.Fset.PositionFor(file.Pos(), false).Filename
		path, ok := insideRoot(root, filename)
		if ok {
			result = append(result, syntaxFile{file: file, filename: filename, path: path})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].path < result[j].path })
	return result
}

func buildCatalog(ctx context.Context, root string, pkgs []*packages.Package) (*catalog, error) {
	result := &catalog{
		packages:    map[string]packageIdentity{},
		functions:   map[*types.Func]functionIdentity{},
		typeObjects: map[*types.TypeName]*namedType{},
	}
	for _, pkg := range pkgs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		files := syntaxFiles(root, pkg)
		candidate := packageIdentity{module: modulePath(pkg), local: len(files) > 0}
		current, exists := result.packages[pkg.PkgPath]
		if !exists || !current.local && candidate.local {
			result.packages[pkg.PkgPath] = candidate
		}
		for _, item := range files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			initOrdinal := 0
			generated := ast.IsGenerated(item.file)
			for _, declaration := range item.file.Decls {
				switch node := declaration.(type) {
				case *ast.GenDecl:
					if node.Tok != token.TYPE {
						continue
					}
					for _, spec := range node.Specs {
						typeSpec, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						object, _ := pkg.TypesInfo.Defs[typeSpec.Name].(*types.TypeName)
						if object == nil {
							continue
						}
						named := namedOf(object.Type())
						var iface *types.Interface
						kind := "type"
						if named != nil {
							iface, _ = named.Underlying().(*types.Interface)
						}
						if iface != nil {
							kind = "interface"
						}
						qualified := pkg.Name + "." + typeSpec.Name.Name
						record := &namedType{
							id: stableID(candidate.module, pkg.PkgPath, kind, "", qualified), path: item.path,
							named: named, interfaceType: iface,
						}
						result.typeObjects[object] = record
						result.types = append(result.types, record)
					}
				case *ast.FuncDecl:
					object, _ := pkg.TypesInfo.Defs[node.Name].(*types.Func)
					if object == nil {
						continue
					}
					receiver := receiverName(object)
					kind := "function"
					qualified := pkg.Name + "." + node.Name.Name
					identityPart := receiver
					if receiver != "" {
						kind = "method"
						qualified = pkg.Name + "." + receiver + "." + node.Name.Name
					} else if node.Name.Name == "init" {
						initOrdinal++
						identityPart = item.path + "#" + strconv.Itoa(initOrdinal)
						qualified = fmt.Sprintf("%s.init[%s#%d]", pkg.Name, item.path, initOrdinal)
					}
					idQualified := qualified
					if node.Name.Name == "init" {
						idQualified = pkg.Name + ".init"
					}
					result.functions[object] = functionIdentity{
						id:   stableID(candidate.module, pkg.PkgPath, kind, identityPart, idQualified),
						path: item.path, qualified: qualified, generated: generated,
					}
				}
			}
		}
	}
	return result, nil
}

func newFileOutput(path string, req Request, hashes map[string]string) *fileOutput {
	return &fileOutput{
		indexed: core.IndexedFile{File: core.FileState{
			Path: path, Kind: "code", Language: "go", Extension: ".go", Hash: hashes[path], ParserVersion: parserVersion, Generation: req.Generation,
		}},
		units: map[string]bool{}, edges: map[string]bool{},
	}
}

func (out *fileOutput) addUnit(unit core.CodeUnit) {
	if !out.units[unit.ID] {
		out.units[unit.ID] = true
		out.indexed.Units = append(out.indexed.Units, unit)
	}
}

func (out *fileOutput) addEdge(edge core.CodeEdge) {
	key := edgeKey(edge)
	if !out.edges[key] {
		out.edges[key] = true
		out.indexed.Edges = append(out.indexed.Edges, edge)
	}
}

func extractDeclarations(ctx context.Context, req Request, hashes map[string]string, current packageData, item syntaxFile, contents []byte, out *fileOutput, identity *catalog) error {
	fileID := stableID(current.module, current.pkg.PkgPath, "file", "", item.path)
	for _, declaration := range item.file.Decls {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch node := declaration.(type) {
		case *ast.GenDecl:
			if node.Tok != token.TYPE {
				continue
			}
			for _, spec := range node.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				object, _ := current.pkg.TypesInfo.Defs[typeSpec.Name].(*types.TypeName)
				record := identity.typeObjects[object]
				if record == nil {
					continue
				}
				kind := "type"
				if record.interfaceType != nil {
					kind = "interface"
				}
				qualified := current.pkg.Name + "." + typeSpec.Name.Name
				id := record.id
				unit := codeUnit(req, hashes, item.path, id, typeSpec.Name.Name, qualified, kind, typeSpec, declarationNode(node, typeSpec), current.pkg.Fset, contents)
				out.addUnit(unit)
				out.addEdge(edgeForNode(req, hashes, item.path, fileID, id, "contains", typeSpec, current.pkg.Fset, "local"))
				record.unit, record.emitted = unit, true
			}
		case *ast.FuncDecl:
			object, _ := current.pkg.TypesInfo.Defs[node.Name].(*types.Func)
			function, ok := identity.functions[object]
			if !ok {
				continue
			}
			receiver := receiverName(object)
			kind := "function"
			parent := fileID
			if receiver != "" {
				kind = "method"
				parent = stableID(current.module, current.pkg.PkgPath, "type", "", current.pkg.Name+"."+receiver)
			}
			out.addUnit(codeUnit(req, hashes, item.path, function.id, node.Name.Name, function.qualified, kind, node, node, current.pkg.Fset, contents))
			out.addEdge(edgeForNode(req, hashes, item.path, parent, function.id, "contains", node, current.pkg.Fset, "local"))
		}
	}
	return nil
}

func declarationNode(declaration *ast.GenDecl, spec *ast.TypeSpec) ast.Node {
	if len(declaration.Specs) == 1 && !declaration.Lparen.IsValid() {
		return declaration
	}
	return spec
}

func extractImports(ctx context.Context, req Request, hashes map[string]string, current packageData, item syntaxFile, out *fileOutput, identity *catalog, imports map[string][]string) error {
	fileID := stableID(current.module, current.pkg.PkgPath, "file", "", item.path)
	for _, spec := range item.file.Imports {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		imports[current.pkg.PkgPath] = append(imports[current.pkg.PkgPath], path)
		packageID, exists := identity.packages[path]
		resolution := "external"
		if exists && packageID.local {
			resolution = "local"
		}
		target := stableID(packageID.module, path, "package", "", path)
		out.addEdge(edgeForNode(req, hashes, item.path, fileID, target, "imports", spec, current.pkg.Fset, resolution))
	}
	return nil
}

func addEmbeds(ctx context.Context, req Request, hashes map[string]string, files map[string]*fileOutput, identity *catalog) error {
	for _, source := range identity.types {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !source.emitted || source.named == nil {
			continue
		}
		var embedded []types.Type
		switch underlying := source.named.Underlying().(type) {
		case *types.Struct:
			for i := 0; i < underlying.NumFields(); i++ {
				if underlying.Field(i).Embedded() {
					embedded = append(embedded, underlying.Field(i).Type())
				}
			}
		case *types.Interface:
			for i := 0; i < underlying.NumEmbeddeds(); i++ {
				embedded = append(embedded, underlying.EmbeddedType(i))
			}
		}
		for _, embeddedType := range embedded {
			named := namedOf(embeddedType)
			if named == nil || named.Obj() == nil {
				continue
			}
			targetRecord := identity.typeObjects[named.Obj()]
			target := externalTypeID(named)
			resolution := "external"
			if targetRecord != nil {
				target = targetRecord.id
				resolution = "local"
			}
			out := files[source.path]
			out.addEdge(edgeFor(req, hashes, source.path, source.id, target, "embeds", source.unit.StartLine, source.unit.EndLine, resolution))
		}
	}
	return nil
}

func addImplements(ctx context.Context, req Request, hashes map[string]string, files map[string]*fileOutput, identity *catalog) error {
	interfaces := []*namedType{}
	concrete := []*namedType{}
	for _, record := range identity.types {
		if record.interfaceType != nil {
			interfaces = append(interfaces, record)
		} else if record.emitted && record.named != nil {
			concrete = append(concrete, record)
		}
	}
	sortNamedTypes(interfaces)
	sortNamedTypes(concrete)
	for _, source := range concrete {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, target := range interfaces {
			methodSet := implementingMethodSet(source.named, target.interfaceType)
			if methodSet != nil && activeMethodProvenance(methodSet, target.interfaceType, identity, files) {
				files[source.path].addEdge(edgeFor(req, hashes, source.path, source.id, target.id, "implements", source.unit.StartLine, source.unit.EndLine, "local"))
			}
		}
	}
	return nil
}

func implementingMethodSet(named *types.Named, iface *types.Interface) *types.MethodSet {
	if types.Implements(named, iface) {
		return types.NewMethodSet(named)
	}
	pointer := types.NewPointer(named)
	if types.Implements(pointer, iface) {
		return types.NewMethodSet(pointer)
	}
	return nil
}

func activeMethodProvenance(set *types.MethodSet, iface *types.Interface, identity *catalog, files map[string]*fileOutput) bool {
	iface.Complete()
	for i := 0; i < iface.NumMethods(); i++ {
		method := iface.Method(i)
		selection := set.Lookup(method.Pkg(), method.Name())
		if selection == nil {
			return false
		}
		object, _ := selection.Obj().(*types.Func)
		provenance, ok := identity.functions[object]
		if !ok || provenance.generated || files[provenance.path] == nil {
			return false
		}
	}
	return true
}

func addCalls(ctx context.Context, req Request, hashes map[string]string, root string, pkg *packages.Package, files map[string]*fileOutput, identity *catalog) (warnings []string, err error) {
	defer func() {
		if value := recover(); value != nil {
			warnings = append(warnings, fmt.Sprintf("%s: call graph unavailable: %v", pkg.PkgPath, value))
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	program, _ := ssautil.Packages([]*packages.Package{pkg}, ssa.InstantiateGenerics)
	program.Build()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	graph := vta.CallGraph(ssautil.AllFunctions(program), nil)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, node := range graph.Nodes {
		for _, call := range node.Out {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if call.Site == nil || call.Caller == nil || call.Callee == nil {
				continue
			}
			position := program.Fset.PositionFor(call.Site.Pos(), false)
			path, ok := insideRoot(root, position.Filename)
			if !ok || files[path] == nil {
				continue
			}
			sourceObject := callerFunctionObject(call.Caller.Func)
			if sourceObject == nil {
				continue
			}
			sourceFunction, ok := identity.functions[sourceObject]
			if !ok || files[sourceFunction.path] == nil {
				continue
			}
			targetObject, _ := call.Callee.Func.Object().(*types.Func)
			if targetObject == nil {
				continue
			}
			targetFunction, local := identity.functions[targetObject]
			target, resolution := targetFunction.id, "local"
			if !local {
				target = externalFunctionID(call.Callee.Func)
				resolution = "external"
			}
			line := uint32(position.Line)
			files[path].addEdge(edgeFor(req, hashes, path, sourceFunction.id, target, "calls", line, line, resolution))
		}
	}
	return warnings, nil
}

func callerFunctionObject(function *ssa.Function) *types.Func {
	for function != nil {
		if object, ok := function.Object().(*types.Func); ok {
			return object
		}
		function = function.Parent()
	}
	return nil
}

func receiverName(object types.Object) string {
	function, _ := object.(*types.Func)
	if function == nil {
		return ""
	}
	signature, _ := function.Type().(*types.Signature)
	if signature == nil || signature.Recv() == nil {
		return ""
	}
	return namedTypeName(signature.Recv().Type())
}

func namedTypeName(value types.Type) string {
	named := namedOf(value)
	if named == nil || named.Obj() == nil {
		return ""
	}
	return named.Obj().Name()
}

func namedOf(value types.Type) *types.Named {
	for {
		switch current := value.(type) {
		case *types.Pointer:
			value = current.Elem()
		case *types.Alias:
			value = types.Unalias(current)
		default:
			named, _ := value.(*types.Named)
			return named
		}
	}
}

func codeUnit(req Request, hashes map[string]string, path, id, name, qualified, kind string, lines, source ast.Node, fset *token.FileSet, contents []byte) core.CodeUnit {
	start, end := nodeLines(fset, lines)
	return core.CodeUnit{
		ID: id, Name: name, QualifiedName: qualified, Kind: kind, Language: "go", Extension: ".go", Path: path,
		Source: sourceText(fset, source, contents), FileHash: hashes[path], StartLine: start, EndLine: end,
		Generation: req.Generation, Weight: sourceWeight(path),
	}
}

func edgeForNode(req Request, hashes map[string]string, path, source, target, relation string, node ast.Node, fset *token.FileSet, resolution string) core.CodeEdge {
	start, end := nodeLines(fset, node)
	return edgeFor(req, hashes, path, source, target, relation, start, end, resolution)
}

func edgeFor(req Request, hashes map[string]string, path, source, target, relation string, start, end uint32, resolution string) core.CodeEdge {
	return core.CodeEdge{SourceID: source, TargetID: target, Relation: relation, Path: path, FileHash: hashes[path], Resolution: resolution, StartLine: start, EndLine: end, Generation: req.Generation}
}

func nodeLines(fset *token.FileSet, node ast.Node) (uint32, uint32) {
	return uint32(fset.PositionFor(node.Pos(), false).Line), uint32(fset.PositionFor(node.End(), false).Line)
}

func sourceText(fset *token.FileSet, node ast.Node, contents []byte) string {
	file := fset.File(node.Pos())
	if file == nil {
		return ""
	}
	start, end := file.Offset(node.Pos()), file.Offset(node.End())
	if start < 0 || end < start || end > len(contents) {
		return ""
	}
	return string(contents[start:end])
}

func sourceWeight(path string) float64 {
	if strings.HasSuffix(path, "_test.go") {
		return 0.6
	}
	return 1
}

func stableID(module, packagePath, kind, receiver, qualified string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{module, packagePath, kind, receiver, qualified}, "\x00")))
	return fmt.Sprintf("go:%x", sum)
}

func externalTypeID(named *types.Named) string {
	object := named.Obj()
	return stableID("external", objectPackage(object), "type", "", object.Name())
}

func externalFunctionID(function *ssa.Function) string {
	if object, ok := function.Object().(*types.Func); ok {
		return stableID("external", objectPackage(object), "function", namedTypeName(receiverType(object)), object.FullName())
	}
	return stableID("external", function.String(), "function", "", function.String())
}

func receiverType(function *types.Func) types.Type {
	signature, _ := function.Type().(*types.Signature)
	if signature != nil && signature.Recv() != nil {
		return signature.Recv().Type()
	}
	return nil
}

func objectPackage(object types.Object) string {
	if object == nil || object.Pkg() == nil {
		return ""
	}
	return object.Pkg().Path()
}

func modulePath(pkg *packages.Package) string {
	if pkg != nil && pkg.Module != nil {
		return pkg.Module.Path
	}
	if pkg != nil {
		return pkg.PkgPath
	}
	return ""
}

func packageForPath(pkgs []*packages.Package, path string) *packages.Package {
	for _, pkg := range pkgs {
		if pkg.PkgPath == path {
			return pkg
		}
	}
	return nil
}

func packageFilePaths(files map[string]string, packagePath string) []string {
	result := []string{}
	for path, owner := range files {
		if owner == packagePath {
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func sortNamedTypes(values []*namedType) {
	sort.Slice(values, func(i, j int) bool { return values[i].id < values[j].id })
}

func edgeKey(edge core.CodeEdge) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%010d\x00%010d", edge.SourceID, edge.Relation, edge.TargetID, edge.Path, edge.StartLine, edge.EndLine)
}

func sortedFileKeys(files map[string]*fileOutput) []string {
	result := make([]string, 0, len(files))
	for path := range files {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func canonicalRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("project root must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("canonicalize project root: %w", err)
	}
	return resolved, nil
}

func validateHashes(root string, values map[string]string) (map[string]string, error) {
	if values == nil {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		hash := values[path]
		clean, err := safePath(path)
		if err != nil {
			return nil, fmt.Errorf("invalid active path %q: %w", path, err)
		}
		if existing, ok := result[clean]; ok {
			if existing != hash {
				return nil, fmt.Errorf("conflicting active hashes for normalized path %q", clean)
			}
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(clean))
		if _, err := os.Lstat(full); err == nil {
			if _, ok := insideRoot(root, full); !ok {
				return nil, fmt.Errorf("active path %q escapes project root", path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("validate active path %q: %w", path, err)
		}
		result[clean] = hash
	}
	return result, nil
}

func validatePatterns(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{"./..."}, nil
	}
	result := make([]string, 0, len(values))
	for _, pattern := range values {
		if pattern == "" || filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "file=") {
			return nil, fmt.Errorf("unsafe package pattern %q", pattern)
		}
		if strings.HasPrefix(pattern, ".") {
			trimmed := strings.TrimSuffix(pattern, "/...")
			if _, err := safePath(strings.TrimPrefix(trimmed, "./")); err != nil && trimmed != "." {
				return nil, fmt.Errorf("unsafe package pattern %q", pattern)
			}
		}
		result = append(result, pattern)
	}
	return uniqueSorted(result), nil
}

func safePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errors.New("path is absolute")
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." || path == ".." || strings.HasPrefix(path, "../") {
		return "", errors.New("path escapes project root")
	}
	return path, nil
}

func insideRoot(root, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	path, err = safePath(relative)
	return path, err == nil
}

func allowedFile(path string, hashes map[string]string) bool {
	_, ok := hashes[path]
	return hashes == nil || ok
}

func loadPackages(ctx context.Context, root string, patterns []string) (packageLoad, []string, error) {
	moduleDirs, err := findModuleDirs(ctx, root)
	if err != nil {
		return packageLoad{}, nil, err
	}
	if len(moduleDirs) == 0 {
		moduleDirs = []string{root}
	}
	loaded := map[string]*packages.Package{}
	warnings := []string{}
	for _, dir := range moduleDirs {
		if err := ctx.Err(); err != nil {
			return packageLoad{}, warnings, err
		}
		localPatterns := patternsForModule(root, dir, patterns)
		if len(localPatterns) == 0 {
			continue
		}
		cfg := &packages.Config{
			Context: ctx,
			Dir:     dir,
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
				packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
				packages.NeedImports | packages.NeedDeps | packages.NeedModule | packages.NeedForTest,
			Tests: true,
			Env:   packageEnvironment(os.Environ(), false),
		}
		pkgs, loadErr := packages.Load(cfg, localPatterns...)
		if err := ctx.Err(); err != nil {
			return packageLoad{}, warnings, err
		}
		if !hasSyntax(pkgs) {
			cfg.Env = packageEnvironment(os.Environ(), true)
			if independent, independentErr := packages.Load(cfg, localPatterns...); hasSyntax(independent) {
				pkgs, loadErr = independent, independentErr
			}
			if err := ctx.Err(); err != nil {
				return packageLoad{}, warnings, err
			}
		}
		if loadErr != nil {
			if ctx.Err() != nil {
				return packageLoad{}, warnings, ctx.Err()
			}
			warnings = append(warnings, cleanWarning(root, loadErr.Error()))
		}
		for _, pkg := range pkgs {
			if strings.HasSuffix(pkg.PkgPath, ".test") {
				continue
			}
			loaded[pkg.ID] = pkg
		}
	}
	result := make([]*packages.Package, 0, len(loaded))
	for _, pkg := range loaded {
		result = append(result, pkg)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].PkgPath != result[j].PkgPath {
			return result[i].PkgPath < result[j].PkgPath
		}
		if (result[i].ForTest == "") != (result[j].ForTest == "") {
			return result[i].ForTest == ""
		}
		return result[i].ID < result[j].ID
	})
	all, err := collectPackages(ctx, result)
	if err != nil {
		return packageLoad{}, warnings, err
	}
	return packageLoad{selected: result, all: all}, warnings, nil
}

func collectPackages(ctx context.Context, selected []*packages.Package) ([]*packages.Package, error) {
	seen := map[*packages.Package]bool{}
	result := []*packages.Package{}
	var visit func(*packages.Package) error
	visit = func(pkg *packages.Package) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pkg == nil || seen[pkg] {
			return nil
		}
		seen[pkg] = true
		result = append(result, pkg)
		paths := make([]string, 0, len(pkg.Imports))
		for path := range pkg.Imports {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			if err := visit(pkg.Imports[path]); err != nil {
				return err
			}
		}
		return nil
	}
	for _, pkg := range selected {
		if err := visit(pkg); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func hasSyntax(pkgs []*packages.Package) bool {
	for _, pkg := range pkgs {
		if len(pkg.Syntax) > 0 {
			return true
		}
	}
	return false
}

func packageEnvironment(environment []string, independent bool) []string {
	result := make([]string, 0, len(environment)+1)
	hasProxy, hasWorkspace := false, false
	for _, value := range environment {
		if strings.HasPrefix(value, "GOPROXY=") {
			result = append(result, "GOPROXY=off")
			hasProxy = true
			continue
		}
		if strings.HasPrefix(value, "GOWORK=") {
			hasWorkspace = true
			if independent {
				value = "GOWORK=off"
			}
		}
		result = append(result, value)
	}
	if !hasProxy {
		result = append(result, "GOPROXY=off")
	}
	if independent && !hasWorkspace {
		result = append(result, "GOWORK=off")
	}
	return result
}

func findModuleDirs(ctx context.Context, root string) ([]string, error) {
	result := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path != root && entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
		}
		if !entry.IsDir() && entry.Name() == "go.mod" {
			if _, ok := insideRoot(root, path); ok {
				result = append(result, filepath.Dir(path))
			}
		}
		return nil
	})
	sort.Strings(result)
	return result, err
}

func patternsForModule(root, moduleDir string, patterns []string) []string {
	result := []string{}
	moduleRelative, _ := filepath.Rel(root, moduleDir)
	for _, pattern := range patterns {
		if !strings.HasPrefix(pattern, "./") || moduleRelative == "." {
			result = append(result, pattern)
			continue
		}
		ellipsis := strings.HasSuffix(pattern, "/...")
		target := strings.TrimPrefix(strings.TrimSuffix(pattern, "/..."), "./")
		if target == "" || target == "." {
			result = append(result, "./...")
			continue
		}
		relative, err := filepath.Rel(moduleRelative, filepath.FromSlash(target))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		local := "./" + filepath.ToSlash(relative)
		if relative == "." {
			local = "."
		}
		if ellipsis {
			local = strings.TrimSuffix(local, "/") + "/..."
		}
		result = append(result, local)
	}
	return uniqueSorted(result)
}

func cleanWarning(root, warning string) string {
	return strings.ReplaceAll(warning, root+string(filepath.Separator), "")
}
