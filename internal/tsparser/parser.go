package tsparser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/ratibordas/go-mcp-codeweft/internal/config"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/project"
)

const parserVersion = "1"

type Request struct {
	Root       string
	Paths      []string
	Generation uint64
	FileHashes map[string]string
}

type Result struct {
	Files                []core.IndexedFile
	ModuleImports        map[string][]string
	ReverseModuleImports map[string][]string
	Warnings             []string
}

type Parser struct{}

func New() *Parser { return &Parser{} }

type syntaxFile struct {
	path, extension, language string
	source                    []byte
	tree                      *tree_sitter.Tree
	warnings                  []string
}

type declaration struct {
	path, name, qualified, kind, owner string
	id, source                         string
	start, end                         uint32
	exported, defaultExport, callable  bool
	node, callNode                     *tree_sitter.Node
}

type catalog struct {
	declarations map[string][]*declaration
	byName       map[string]map[string][]*declaration
	defaults     map[string]*declaration
	files        map[string]*syntaxFile
	resolver     *resolver
	ambiguous    map[string]bool
}

type importBinding struct {
	target, imported, specifier, resolution string
	namespace                               bool
}

type fileOutput struct {
	indexed core.IndexedFile
	units   map[string]bool
	edges   map[string]bool
}

func languageFor(extension string) *tree_sitter.Language {
	switch strings.ToLower(extension) {
	case ".tsx":
		return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX())
	case ".ts", ".d.ts":
		return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript())
	case ".js", ".jsx", ".mjs", ".cjs":
		return tree_sitter.NewLanguage(tree_sitter_javascript.Language())
	default:
		return nil
	}
}

func (p *Parser) Parse(ctx context.Context, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	root, err := canonicalRoot(req.Root)
	if err != nil {
		return Result{}, err
	}
	hashes, err := validateHashes(ctx, root, req.FileHashes)
	if err != nil {
		return Result{}, err
	}
	paths, err := sourcePaths(ctx, root, hashes, req.Paths)
	if err != nil {
		return Result{}, err
	}
	selected, err := selectedPaths(root, req.Paths, paths, hashes)
	if err != nil {
		return Result{}, err
	}
	resolver, err := newResolver(root)
	if err != nil {
		return Result{}, err
	}
	if hashes != nil || len(req.Paths) == 0 {
		resolver.withAllowed(paths)
	} else {
		resolver.withPolicy(func(path string) bool {
			_, reason, err := project.InspectWithIndex(root, path, config.Index{})
			return err == nil && reason == ""
		})
	}

	files := make(map[string]*syntaxFile, len(paths))
	warnings := []string{}
	queue := append([]string(nil), selected...)
	queued := make(map[string]bool, len(queue))
	for _, path := range queue {
		queued[path] = true
	}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		if err := ctx.Err(); err != nil {
			closeSyntaxFiles(files)
			return Result{}, err
		}
		file, parseWarnings, err := parseFile(root, path)
		if err != nil {
			warnings = append(warnings, path+": "+err.Error())
			continue
		}
		files[path] = file
		warnings = append(warnings, parseWarnings...)
		for _, dependency := range localModuleDependencies(file, resolver) {
			if !queued[dependency] {
				queued[dependency] = true
				queue = append(queue, dependency)
			}
		}
	}
	defer closeSyntaxFiles(files)
	identity, err := buildCatalog(ctx, files, resolver)
	if err != nil {
		return Result{}, err
	}
	result := Result{ModuleImports: map[string][]string{}, ReverseModuleImports: map[string][]string{}}

	for _, path := range selected {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		file := files[path]
		if file == nil {
			continue
		}
		out := newFileOutput(root, file, req, hashes)
		result.ReverseModuleImports[path] = nil
		moduleID := moduleID(path)
		out.addUnit(unitFor(file, req, hashes, &declaration{
			path: path, name: filepath.Base(path), qualified: path, kind: "file", id: moduleID,
			source: string(file.source), start: 1, end: fileEndLine(file.source),
		}))
		for _, declaration := range identity.declarations[path] {
			out.addUnit(unitFor(file, req, hashes, declaration))
			out.addEdge(edgeFor(file, req, hashes, moduleID, declaration.id, "contains", declaration.start, declaration.end, "local"))
			if declaration.exported && declaration.owner == "" {
				out.addEdge(edgeFor(file, req, hashes, moduleID, declaration.id, "exports", declaration.start, declaration.end, "local"))
			}
		}
		out.indexed.Warnings = append(out.indexed.Warnings, file.warnings...)
		bindings, imports, edgeWarnings := extractModuleEdges(file, resolver, identity, out, req, hashes)
		warnings = append(warnings, edgeWarnings...)
		for _, warning := range edgeWarnings {
			out.indexed.Warnings = append(out.indexed.Warnings, warning)
		}
		result.ModuleImports[path] = uniqueSorted(imports)
		heritageWarnings := extractHeritage(file, identity, bindings, out, req, hashes)
		callWarnings := extractCalls(file, identity, bindings, out, req, hashes)
		for _, warning := range append(heritageWarnings, callWarnings...) {
			warnings = append(warnings, warning)
			out.indexed.Warnings = append(out.indexed.Warnings, warning)
		}
		out.indexed.Warnings = uniqueSorted(out.indexed.Warnings)
		sort.Slice(out.indexed.Units, func(i, j int) bool { return out.indexed.Units[i].ID < out.indexed.Units[j].ID })
		sort.Slice(out.indexed.Edges, func(i, j int) bool { return edgeKey(out.indexed.Edges[i]) < edgeKey(out.indexed.Edges[j]) })
		result.Files = append(result.Files, out.indexed)
	}
	for source, targets := range result.ModuleImports {
		for _, target := range targets {
			if strings.HasPrefix(target, "external:") {
				continue
			}
			result.ReverseModuleImports[target] = append(result.ReverseModuleImports[target], source)
		}
	}
	for target, importers := range result.ReverseModuleImports {
		result.ReverseModuleImports[target] = uniqueSorted(importers)
	}
	result.Warnings = uniqueSorted(append(warnings, resolver.Warnings()...))
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return result, nil
}

func parseFile(root, path string) (*syntaxFile, []string, error) {
	full, ok := safeExistingPath(root, path)
	if !ok {
		return nil, nil, errors.New("path is missing or escapes project root")
	}
	source, err := os.ReadFile(full)
	if err != nil {
		return nil, nil, err
	}
	extension := sourceExtension(path)
	language := languageFor(extension)
	if language == nil {
		return nil, nil, fmt.Errorf("unsupported extension %q", extension)
	}
	parser := tree_sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(language); err != nil {
		return nil, nil, err
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		return nil, nil, errors.New("tree-sitter returned no tree")
	}
	file := &syntaxFile{path: path, extension: extension, language: scriptLanguage(extension), source: source, tree: tree}
	warnings := parseErrorWarnings(file)
	file.warnings = warnings
	return file, warnings, nil
}

func parseErrorWarnings(file *syntaxFile) []string {
	root := file.tree.RootNode()
	if !root.HasError() {
		return nil
	}
	warnings := []string{}
	walkNamed(root, func(node *tree_sitter.Node) bool {
		if node.IsError() || node.IsMissing() {
			start, end := nodeLines(node)
			warnings = append(warnings, fmt.Sprintf("%s: parse error at lines %d-%d", file.path, start, end))
		}
		return true
	})
	if len(warnings) == 0 {
		warnings = append(warnings, file.path+": parse error")
	}
	return uniqueSorted(warnings)
}

func buildCatalog(ctx context.Context, files map[string]*syntaxFile, resolver *resolver) (*catalog, error) {
	result := &catalog{declarations: map[string][]*declaration{}, byName: map[string]map[string][]*declaration{}, defaults: map[string]*declaration{}, files: files, resolver: resolver, ambiguous: map[string]bool{}}
	paths := sortedSyntaxPaths(files)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		declarations := declarationsFor(files[path])
		result.declarations[path] = declarations
		result.byName[path] = map[string][]*declaration{}
		for _, declaration := range declarations {
			result.byName[path][declaration.name] = append(result.byName[path][declaration.name], declaration)
			result.byName[path][declaration.qualified] = append(result.byName[path][declaration.qualified], declaration)
			if declaration.defaultExport {
				result.defaults[path] = declaration
			}
		}
	}
	return result, nil
}

func declarationsFor(file *syntaxFile) []*declaration {
	result := []*declaration{}
	root := file.tree.RootNode()
	for i := uint(0); i < root.NamedChildCount(); i++ {
		node := root.NamedChild(i)
		exported, defaultExport := false, false
		if node.Kind() == "export_statement" {
			exported = true
			defaultExport = strings.HasPrefix(strings.TrimSpace(node.Utf8Text(file.source)), "export default")
			if declarationNode := node.ChildByFieldName("declaration"); declarationNode != nil {
				node = declarationNode
			} else if value := node.ChildByFieldName("value"); defaultExport && value != nil && (value.Kind() == "arrow_function" || value.Kind() == "function_expression") {
				kind := "function"
				if isFunctionComponent("default", value) {
					kind = "component"
				}
				result = append(result, newDeclaration(file, value, value, "default", kind, "", true, true, true))
				continue
			} else {
				continue
			}
		}
		result = append(result, declarationsFromNode(file, node, exported, defaultExport)...)
	}
	assignFunctionIDs(file, result)
	sort.Slice(result, func(i, j int) bool { return result[i].id < result[j].id })
	return result
}

func assignFunctionIDs(file *syntaxFile, declarations []*declaration) {
	groups := map[string][]*declaration{}
	for _, declaration := range declarations {
		if declaration.kind == "function" && declaration.owner == "" && declaration.node != nil {
			groups[declaration.name] = append(groups[declaration.name], declaration)
		}
	}
	for name, group := range groups {
		if len(group) == 1 {
			continue
		}
		base := group[0]
		for _, declaration := range group {
			if declaration.node.ChildByFieldName("body") != nil {
				base = declaration
				break
			}
		}
		base.id = stableID(file.path, "function", "", name)
		counts := map[string]int{}
		for _, declaration := range group {
			if declaration == base {
				continue
			}
			discriminator := functionDiscriminator(declaration.node, file.source)
			counts[discriminator]++
			declaration.id = stableID(file.path, "function:"+discriminator+"#"+strconv.Itoa(counts[discriminator]), "", name)
		}
	}
}

func declarationsFromNode(file *syntaxFile, node *tree_sitter.Node, exported, defaultExport bool) []*declaration {
	if node.Kind() == "ambient_declaration" {
		result := []*declaration{}
		for i := uint(0); i < node.NamedChildCount(); i++ {
			result = append(result, declarationsFromNode(file, node.NamedChild(i), exported, defaultExport)...)
		}
		return result
	}
	var name, kind string
	callNode := node
	sourceNode := node
	switch node.Kind() {
	case "class_declaration", "abstract_class_declaration":
		name = fieldText(node, "name", file.source)
		kind = "class"
		if isClassComponent(node, file.source) {
			kind = "component"
		}
	case "interface_declaration":
		name = fieldText(node, "name", file.source)
		kind = "interface"
	case "type_alias_declaration":
		name = fieldText(node, "name", file.source)
		kind = "type"
	case "function_declaration", "generator_function_declaration", "function_signature":
		name = fieldText(node, "name", file.source)
		kind = "function"
		if isFunctionComponent(name, node) {
			kind = "component"
		}
	case "lexical_declaration", "variable_declaration":
		result := []*declaration{}
		for i := uint(0); i < node.NamedChildCount(); i++ {
			declarator := node.NamedChild(i)
			if declarator.Kind() != "variable_declarator" {
				continue
			}
			value := declarator.ChildByFieldName("value")
			if value == nil || value.Kind() != "arrow_function" && value.Kind() != "function_expression" && value.Kind() != "generator_function" {
				continue
			}
			name := fieldText(declarator, "name", file.source)
			if name == "" {
				continue
			}
			kind := "function"
			if isFunctionComponent(name, value) {
				kind = "component"
			}
			result = append(result, newDeclaration(file, declarator, value, name, kind, "", exported, defaultExport, true))
		}
		return result
	default:
		if common := commonJSDeclaration(file, node); common != nil {
			return []*declaration{common}
		}
		return nil
	}
	if name == "" {
		name = "default"
	}
	decl := newDeclaration(file, sourceNode, callNode, name, kind, "", exported, defaultExport, kind == "function" || kind == "component" && strings.Contains(node.Kind(), "function"))
	result := []*declaration{decl}
	if kind == "class" || kind == "component" || kind == "interface" {
		methodCounts := map[string]int{}
		walkNamed(node, func(child *tree_sitter.Node) bool {
			if child.Id() == node.Id() {
				return true
			}
			if child.Kind() != "method_definition" && child.Kind() != "method_signature" && child.Kind() != "abstract_method_signature" {
				return true
			}
			methodName := fieldText(child, "name", file.source)
			if methodName != "" {
				decl := newDeclaration(file, child, child, methodName, "method", name, exported, false, true)
				discriminator := methodDiscriminator(child, file.source)
				methodCounts[discriminator]++
				decl.id = stableID(file.path, "method:"+discriminator+"#"+strconv.Itoa(methodCounts[discriminator]), name, methodName)
				result = append(result, decl)
			}
			return false
		})
	}
	return result
}

func methodDiscriminator(node *tree_sitter.Node, source []byte) string {
	text := node.Utf8Text(source)
	if brace := strings.IndexByte(text, '{'); brace >= 0 {
		text = text[:brace]
	}
	text = strings.Join(strings.Fields(text), "")
	if strings.HasPrefix(text, "static") {
		return "static:" + text
	}
	return "instance:" + text
}

func functionDiscriminator(node *tree_sitter.Node, source []byte) string {
	text := node.Utf8Text(source)
	if brace := strings.IndexByte(text, '{'); brace >= 0 {
		text = text[:brace]
	}
	return strings.Join(strings.Fields(text), "")
}

func commonJSDeclaration(file *syntaxFile, node *tree_sitter.Node) *declaration {
	if node.Kind() != "expression_statement" {
		return nil
	}
	assignment := firstDescendant(node, "assignment_expression")
	if assignment == nil {
		return nil
	}
	left, right := assignment.ChildByFieldName("left"), assignment.ChildByFieldName("right")
	if left == nil || right == nil || right.Kind() != "function_expression" && right.Kind() != "arrow_function" {
		return nil
	}
	leftText := left.Utf8Text(file.source)
	if leftText != "module.exports" && !strings.HasPrefix(leftText, "exports.") {
		return nil
	}
	name := fieldText(right, "name", file.source)
	if name == "" {
		name = strings.TrimPrefix(leftText, "exports.")
		if name == "module.exports" {
			name = "default"
		}
	}
	return newDeclaration(file, node, right, name, "function", "", true, leftText == "module.exports", true)
}

func newDeclaration(file *syntaxFile, sourceNode, callNode *tree_sitter.Node, name, kind, owner string, exported, defaultExport, callable bool) *declaration {
	qualified := name
	if owner != "" {
		qualified = owner + "." + name
	}
	start, end := nodeLines(sourceNode)
	return &declaration{
		path: file.path, name: name, qualified: qualified, kind: kind, owner: owner,
		id: stableID(file.path, kind, owner, name), source: sourceNode.Utf8Text(file.source), start: start, end: end,
		exported: exported, defaultExport: defaultExport, callable: callable, node: sourceNode, callNode: callNode,
	}
}

func extractModuleEdges(file *syntaxFile, resolver *resolver, identity *catalog, out *fileOutput, req Request, hashes map[string]string) (map[string]importBinding, []string, []string) {
	bindings := map[string]importBinding{}
	imports, warnings := []string{}, []string{}
	module := moduleID(file.path)
	root := file.tree.RootNode()
	for i := uint(0); i < root.NamedChildCount(); i++ {
		node := root.NamedChild(i)
		switch node.Kind() {
		case "import_statement":
			source := node.ChildByFieldName("source")
			if source == nil {
				continue
			}
			specifier := stringLiteral(source.Utf8Text(file.source))
			ref := resolveModule(resolver, file.path, specifier)
			addImportEdge(file, out, req, hashes, module, node, ref)
			imports = appendImport(imports, ref)
			if ref.resolution == "unresolved" {
				warnings = append(warnings, fmt.Sprintf("%s: unresolved import %s at line %d", file.path, specifier, nodeLinesStart(node)))
			}
			collectImportBindings(node, file.source, ref, bindings)
		case "export_statement":
			exportWarnings, exportImports := addExportEdges(file, node, resolver, identity, out, req, hashes)
			warnings = append(warnings, exportWarnings...)
			imports = append(imports, exportImports...)
		}
	}
	walkNamed(root, func(node *tree_sitter.Node) bool {
		if node.Kind() != "call_expression" {
			return true
		}
		function := node.ChildByFieldName("function")
		arguments := node.ChildByFieldName("arguments")
		if function == nil || arguments == nil {
			return true
		}
		name := function.Utf8Text(file.source)
		if name != "require" && function.Kind() != "import" {
			return true
		}
		argument := firstNamedChild(arguments)
		if argument == nil || argument.Kind() != "string" {
			kind := "require"
			if function.Kind() == "import" {
				kind = "import"
			}
			warning := fmt.Sprintf("%s: unresolved dynamic %s at line %d", file.path, kind, nodeLinesStart(node))
			warnings = append(warnings, warning)
			start, end := nodeLines(node)
			out.addEdge(edgeFor(file, req, hashes, module, "", "imports", start, end, "unresolved"))
			return true
		}
		ref := resolveModule(resolver, file.path, stringLiteral(argument.Utf8Text(file.source)))
		addImportEdge(file, out, req, hashes, module, node, ref)
		imports = appendImport(imports, ref)
		if ref.resolution == "unresolved" {
			warnings = append(warnings, fmt.Sprintf("%s: unresolved import %s at line %d", file.path, ref.specifier, nodeLinesStart(node)))
		}
		if name == "require" {
			if declarator := ancestor(node, "variable_declarator"); declarator != nil {
				local := fieldText(declarator, "name", file.source)
				if local != "" {
					bindings[local] = importBinding{target: ref.target, imported: "*", specifier: ref.specifier, resolution: ref.resolution, namespace: true}
				}
			}
		}
		return true
	})
	return bindings, uniqueSorted(imports), uniqueSorted(warnings)
}

func localModuleDependencies(file *syntaxFile, resolver *resolver) []string {
	paths := []string{}
	add := func(specifier string) {
		if path, ok := resolver.Resolve(file.path, specifier); ok {
			paths = append(paths, path)
		}
	}
	root := file.tree.RootNode()
	for i := uint(0); i < root.NamedChildCount(); i++ {
		node := root.NamedChild(i)
		if node.Kind() != "import_statement" && node.Kind() != "export_statement" {
			continue
		}
		if source := node.ChildByFieldName("source"); source != nil {
			add(stringLiteral(source.Utf8Text(file.source)))
		}
	}
	walkNamed(root, func(node *tree_sitter.Node) bool {
		if node.Kind() != "call_expression" {
			return true
		}
		function, arguments := node.ChildByFieldName("function"), node.ChildByFieldName("arguments")
		if function == nil || arguments == nil || function.Utf8Text(file.source) != "require" && function.Kind() != "import" {
			return true
		}
		argument := firstNamedChild(arguments)
		if argument != nil && argument.Kind() == "string" {
			add(stringLiteral(argument.Utf8Text(file.source)))
		}
		return true
	})
	return uniqueSorted(paths)
}

type moduleReference struct {
	specifier, target, resolution string
}

func resolveModule(resolver *resolver, importer, specifier string) moduleReference {
	if target, ok := resolver.Resolve(importer, specifier); ok {
		return moduleReference{specifier: specifier, target: target, resolution: "local"}
	}
	if isBareSpecifier(specifier) {
		return moduleReference{specifier: specifier, target: "external:" + specifier, resolution: "external"}
	}
	return moduleReference{specifier: specifier, resolution: "unresolved"}
}

func addImportEdge(file *syntaxFile, out *fileOutput, req Request, hashes map[string]string, source string, node *tree_sitter.Node, ref moduleReference) {
	target := ""
	if ref.resolution == "local" {
		target = moduleID(ref.target)
	} else if ref.resolution == "external" {
		target = externalID(ref.specifier, "")
	}
	start, end := nodeLines(node)
	out.addEdge(edgeFor(file, req, hashes, source, target, "imports", start, end, ref.resolution))
}

func addExportEdges(file *syntaxFile, node *tree_sitter.Node, resolver *resolver, identity *catalog, out *fileOutput, req Request, hashes map[string]string) ([]string, []string) {
	warnings, imports := []string{}, []string{}
	module := moduleID(file.path)
	if declarationNode := node.ChildByFieldName("declaration"); declarationNode != nil {
		for _, declaration := range identity.declarations[file.path] {
			if declaration.owner == "" && declaration.node.Id() == declarationNode.Id() {
				start, end := nodeLines(node)
				out.addEdge(edgeFor(file, req, hashes, module, declaration.id, "exports", start, end, "local"))
			}
		}
		return warnings, imports
	}
	source := node.ChildByFieldName("source")
	ref := moduleReference{}
	if source != nil {
		ref = resolveModule(resolver, file.path, stringLiteral(source.Utf8Text(file.source)))
		addImportEdge(file, out, req, hashes, module, node, ref)
		imports = appendImport(imports, ref)
		if ref.resolution == "unresolved" {
			warnings = append(warnings, fmt.Sprintf("%s: unresolved export source %s at line %d", file.path, ref.specifier, nodeLinesStart(node)))
		}
	}
	specifiers := descendantsOfKind(node, "export_specifier")
	if len(specifiers) == 0 && source != nil {
		target := ""
		if ref.resolution == "local" {
			target = moduleID(ref.target)
		} else if ref.resolution == "external" {
			target = externalID(ref.specifier, "")
		}
		start, end := nodeLines(node)
		out.addEdge(edgeFor(file, req, hashes, module, target, "exports", start, end, ref.resolution))
		return warnings, imports
	}
	for _, specifier := range specifiers {
		name := fieldText(specifier, "name", file.source)
		target, resolution := "", "unresolved"
		if source != nil {
			target, resolution = resolveExportedSymbol(identity, ref, name)
		} else if declaration := identity.symbol(file.path, name, false, false); declaration != nil {
			target, resolution = declaration.id, "local"
		}
		start, end := nodeLines(specifier)
		out.addEdge(edgeFor(file, req, hashes, module, target, "exports", start, end, resolution))
		if resolution == "unresolved" {
			warnings = append(warnings, fmt.Sprintf("%s: unresolved export %s at line %d", file.path, name, start))
		}
	}
	value := node.ChildByFieldName("value")
	if value != nil && value.Kind() == "identifier" {
		name := value.Utf8Text(file.source)
		if declaration := identity.symbol(file.path, name, false, false); declaration != nil {
			start, end := nodeLines(node)
			out.addEdge(edgeFor(file, req, hashes, module, declaration.id, "exports", start, end, "local"))
		}
	}
	return warnings, imports
}

func resolveExportedSymbol(identity *catalog, ref moduleReference, name string) (string, string) {
	if ref.resolution == "external" {
		return externalID(ref.specifier, name), "external"
	}
	if ref.resolution != "local" {
		return "", "unresolved"
	}
	if declaration := identity.exported(ref.target, name, map[string]bool{}); declaration != nil {
		return declaration.id, "local"
	}
	return "", "unresolved"
}

func collectImportBindings(node *tree_sitter.Node, source []byte, ref moduleReference, bindings map[string]importBinding) {
	clause := firstDescendant(node, "import_clause")
	if clause == nil {
		return
	}
	for i := uint(0); i < clause.NamedChildCount(); i++ {
		child := clause.NamedChild(i)
		switch child.Kind() {
		case "identifier":
			local := child.Utf8Text(source)
			bindings[local] = importBinding{target: ref.target, imported: "default", specifier: ref.specifier, resolution: ref.resolution}
		case "namespace_import":
			identifier := firstNamedChild(child)
			if identifier != nil {
				local := identifier.Utf8Text(source)
				bindings[local] = importBinding{target: ref.target, imported: "*", specifier: ref.specifier, resolution: ref.resolution, namespace: true}
			}
		case "named_imports":
			for _, specifier := range descendantsOfKind(child, "import_specifier") {
				imported := fieldText(specifier, "name", source)
				local := fieldText(specifier, "alias", source)
				if local == "" {
					local = imported
				}
				bindings[local] = importBinding{target: ref.target, imported: imported, specifier: ref.specifier, resolution: ref.resolution}
			}
		}
	}
}

func extractHeritage(file *syntaxFile, identity *catalog, bindings map[string]importBinding, out *fileOutput, req Request, hashes map[string]string) []string {
	warnings := []string{}
	for _, declaration := range identity.declarations[file.path] {
		if declaration.owner != "" || declaration.kind != "class" && declaration.kind != "component" && declaration.kind != "interface" {
			continue
		}
		walkNamed(declaration.node, func(node *tree_sitter.Node) bool {
			if node.Id() != declaration.node.Id() && (node.Kind() == "class_body" || node.Kind() == "interface_body" || node.Kind() == "object_type") {
				return false
			}
			relation := ""
			switch node.Kind() {
			case "extends_clause", "extends_type_clause":
				relation = "extends"
			case "implements_clause":
				relation = "implements"
			default:
				return true
			}
			for i := uint(0); i < node.NamedChildCount(); i++ {
				targetNode := node.NamedChild(i)
				name := baseReference(targetNode.Utf8Text(file.source))
				target, resolution := resolveReference(identity, bindings, file.path, "", name)
				start, end := nodeLines(targetNode)
				out.addEdge(edgeFor(file, req, hashes, declaration.id, target, relation, start, end, resolution))
				if resolution == "unresolved" {
					warnings = append(warnings, fmt.Sprintf("%s: unresolved %s %s at line %d", file.path, relation, name, start))
				}
			}
			return false
		})
	}
	return uniqueSorted(warnings)
}

func extractCalls(file *syntaxFile, identity *catalog, bindings map[string]importBinding, out *fileOutput, req Request, hashes map[string]string) []string {
	warnings := []string{}
	for _, declaration := range identity.declarations[file.path] {
		if !declaration.callable || declaration.callNode == nil {
			continue
		}
		warnings = append(warnings, callsInNode(file, declaration.callNode, declaration.callNode.Id(), declaration.id, declaration.owner, identity, bindings, out, req, hashes)...)
	}
	warnings = append(warnings, callsInNode(file, file.tree.RootNode(), file.tree.RootNode().Id(), moduleID(file.path), "", identity, bindings, out, req, hashes)...)
	return uniqueSorted(warnings)
}

func callsInNode(file *syntaxFile, root *tree_sitter.Node, rootID uintptr, sourceID, owner string, identity *catalog, bindings map[string]importBinding, out *fileOutput, req Request, hashes map[string]string) []string {
	warnings := []string{}
	var visit func(*tree_sitter.Node)
	visit = func(node *tree_sitter.Node) {
		if node.Id() != rootID && isCallableBoundary(node.Kind()) {
			return
		}
		if node.Kind() == "class_declaration" || node.Kind() == "abstract_class_declaration" {
			if node.Id() != rootID {
				return
			}
		}
		if node.Kind() == "call_expression" {
			function := node.ChildByFieldName("function")
			if function != nil && function.Utf8Text(file.source) != "require" && function.Kind() != "import" {
				lexical := function.Utf8Text(file.source)
				target, resolution := "", "unresolved"
				if !shadowedCallableName(root, rootID, function, file.source, bindings) {
					target, resolution = resolveCall(identity, bindings, file.path, owner, function, file.source)
				}
				start, end := nodeLines(node)
				edge := edgeFor(file, req, hashes, sourceID, target, "calls", start, end, resolution)
				out.addCallEdge(edge, node, lexical)
				if resolution == "unresolved" {
					warnings = append(warnings, fmt.Sprintf("%s: unresolved call %s at line %d", file.path, lexical, start))
				}
			}
		}
		for i := uint(0); i < node.NamedChildCount(); i++ {
			visit(node.NamedChild(i))
		}
	}
	visit(root)
	return warnings
}

func shadowedCallableName(root *tree_sitter.Node, rootID uintptr, function *tree_sitter.Node, source []byte, bindings map[string]importBinding) bool {
	if function.Kind() != "identifier" {
		return false
	}
	name := function.Utf8Text(source)
	for node := function; node != nil; node = node.Parent() {
		if isCallableBoundary(node.Kind()) || node.Kind() == "class_declaration" {
			if bindingContains(firstDescendant(node, "formal_parameters"), name, source) {
				return true
			}
			if hoistedBindingContains(node, name, source) {
				return true
			}
		} else if node.Kind() == "program" || node.Kind() == "module" {
			if _, imported := bindings[name]; imported && hoistedBindingContains(node, name, source) {
				return true
			}
		}
		if node.Kind() == "catch_clause" {
			parameter := node.ChildByFieldName("parameter")
			if parameter == nil && node.NamedChildCount() > 0 {
				parameter = node.NamedChild(0)
			}
			if bindingContains(parameter, name, source) {
				return true
			}
		}
		if node.Kind() == "statement_block" && scopeContainsBinding(node, name, source) {
			return true
		}
		if node.Id() == rootID {
			break
		}
	}
	if _, imported := bindings[name]; !imported {
		return false
	}
	for node := root.Parent(); node != nil; node = node.Parent() {
		if node.Kind() == "program" || node.Kind() == "module" {
			return hoistedBindingContains(node, name, source)
		}
	}
	return false
}

func hoistedBindingContains(root *tree_sitter.Node, name string, source []byte) bool {
	var visit func(*tree_sitter.Node, bool) bool
	visit = func(node *tree_sitter.Node, nested bool) bool {
		if nested {
			return false
		}
		if node.Id() != root.Id() && isCallableBoundary(node.Kind()) {
			return false
		}
		if node == root && directFunctionBindingContains(root, name, source) {
			return true
		}
		if node.Kind() == "variable_declaration" {
			for i := uint(0); i < node.NamedChildCount(); i++ {
				declarator := node.NamedChild(i)
				if declarator.Kind() == "variable_declarator" && bindingContains(declarator.ChildByFieldName("name"), name, source) {
					return true
				}
			}
		}
		for i := uint(0); i < node.NamedChildCount(); i++ {
			if visit(node.NamedChild(i), false) {
				return true
			}
		}
		return false
	}
	return visit(root, false)
}

func directFunctionBindingContains(root *tree_sitter.Node, name string, source []byte) bool {
	var scope *tree_sitter.Node
	switch root.Kind() {
	case "function_declaration", "generator_function_declaration":
		scope = root.ChildByFieldName("body")
	case "program", "module":
		scope = root
	default:
		return false
	}
	if scope == nil {
		return false
	}
	for i := uint(0); i < scope.NamedChildCount(); i++ {
		child := scope.NamedChild(i)
		if child.Kind() == "export_statement" {
			child = child.ChildByFieldName("declaration")
		}
		if child != nil && (child.Kind() == "function_declaration" || child.Kind() == "generator_function_declaration") && fieldText(child, "name", source) == name {
			return true
		}
	}
	return false
}

func scopeContainsBinding(scope *tree_sitter.Node, name string, source []byte) bool {
	for i := uint(0); i < scope.NamedChildCount(); i++ {
		child := scope.NamedChild(i)
		if child.Kind() == "function_declaration" || child.Kind() == "generator_function_declaration" || child.Kind() == "class_declaration" {
			if fieldText(child, "name", source) == name {
				return true
			}
			continue
		}
		if child.Kind() != "lexical_declaration" && child.Kind() != "variable_declaration" {
			continue
		}
		for j := uint(0); j < child.NamedChildCount(); j++ {
			declarator := child.NamedChild(j)
			if declarator.Kind() == "variable_declarator" && bindingContains(declarator.ChildByFieldName("name"), name, source) {
				return true
			}
		}
	}
	return false
}

func bindingContains(node *tree_sitter.Node, name string, source []byte) bool {
	if node == nil {
		return false
	}
	if node.Kind() == "identifier" && node.Utf8Text(source) == name {
		return true
	}
	if node.Kind() == "assignment_pattern" || node.Kind() == "object_assignment_pattern" {
		return bindingContains(node.ChildByFieldName("left"), name, source)
	}
	if node.Kind() == "rest_pattern" {
		return bindingContains(node.ChildByFieldName("argument"), name, source)
	}
	if node.Kind() == "pair_pattern" {
		value := node.ChildByFieldName("value")
		if value == nil && node.NamedChildCount() > 1 {
			value = node.NamedChild(1)
		}
		return bindingContains(value, name, source)
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		if bindingContains(node.NamedChild(i), name, source) {
			return true
		}
	}
	return false
}

func resolveCall(identity *catalog, bindings map[string]importBinding, path, owner string, function *tree_sitter.Node, source []byte) (string, string) {
	if function.Kind() == "identifier" {
		return resolveReference(identity, bindings, path, "", function.Utf8Text(source))
	}
	if function.Kind() != "member_expression" {
		return "", "unresolved"
	}
	object, property := function.ChildByFieldName("object"), function.ChildByFieldName("property")
	if object == nil || property == nil || property.Kind() == "computed_property_name" {
		return "", "unresolved"
	}
	objectName, method := object.Utf8Text(source), property.Utf8Text(source)
	if objectName == "this" && owner != "" {
		if declaration := identity.qualified(path, owner+"."+method); declaration != nil {
			return declaration.id, "local"
		}
		return "", "unresolved"
	}
	if binding, ok := bindings[objectName]; ok {
		if binding.resolution == "external" {
			return externalID(binding.specifier, method), "external"
		}
		if binding.resolution != "local" {
			return "", "unresolved"
		}
		if binding.namespace {
			if declaration := identity.exported(binding.target, method, map[string]bool{}); declaration != nil {
				return declaration.id, "local"
			}
			return "", "unresolved"
		}
		base := identity.exported(binding.target, binding.imported, map[string]bool{})
		if base != nil {
			if declaration := identity.qualified(base.path, base.name+"."+method); declaration != nil {
				return declaration.id, "local"
			}
		}
		return "", "unresolved"
	}
	if base := identity.symbol(path, objectName, false, false); base != nil {
		if declaration := identity.qualified(path, base.name+"."+method); declaration != nil {
			return declaration.id, "local"
		}
	}
	return "", "unresolved"
}

func resolveReference(identity *catalog, bindings map[string]importBinding, path, owner, name string) (string, string) {
	if owner != "" {
		if declaration := identity.qualified(path, owner+"."+name); declaration != nil {
			return declaration.id, "local"
		}
	}
	if dot := strings.IndexByte(name, '.'); dot > 0 {
		object, member := name[:dot], name[strings.LastIndexByte(name, '.')+1:]
		if binding, ok := bindings[object]; ok {
			if binding.resolution == "external" {
				return externalID(binding.specifier, member), "external"
			}
			if binding.resolution == "local" && binding.namespace {
				if declaration := identity.exported(binding.target, member, map[string]bool{}); declaration != nil {
					return declaration.id, "local"
				}
			}
			return "", "unresolved"
		}
	}
	if declaration := identity.symbol(path, name, false, false); declaration != nil {
		return declaration.id, "local"
	}
	binding, ok := bindings[name]
	if !ok {
		return "", "unresolved"
	}
	if binding.resolution == "external" {
		return externalID(binding.specifier, binding.imported), "external"
	}
	if binding.resolution != "local" || binding.namespace {
		return "", "unresolved"
	}
	if declaration := identity.exported(binding.target, binding.imported, map[string]bool{}); declaration != nil {
		return declaration.id, "local"
	}
	return "", "unresolved"
}

func (c *catalog) symbol(path, name string, defaultExport, exportedOnly bool) *declaration {
	if defaultExport {
		return c.defaults[path]
	}
	var fallback *declaration
	for _, declaration := range c.byName[path][name] {
		if declaration.owner == "" && (!exportedOnly || declaration.exported && !declaration.defaultExport) {
			if declaration.kind == "function" && declaration.id == stableID(path, "function", "", name) {
				return declaration
			}
			if fallback == nil {
				fallback = declaration
			}
		}
	}
	return fallback
}

func (c *catalog) qualified(path, name string) *declaration {
	for _, declaration := range c.byName[path][name] {
		if declaration.qualified == name {
			return declaration
		}
	}
	return nil
}

func (c *catalog) exported(path, name string, seen map[string]bool) *declaration {
	key := path + "\x00" + name
	if seen[key] {
		return nil
	}
	seen[key] = true
	if declaration := c.symbol(path, name, name == "default", true); declaration != nil {
		return declaration
	}
	file := c.files[path]
	if file == nil {
		return nil
	}
	root := file.tree.RootNode()
	for i := uint(0); i < root.NamedChildCount(); i++ {
		node := root.NamedChild(i)
		if node.Kind() != "export_statement" {
			continue
		}
		for _, specifier := range descendantsOfKind(node, "export_specifier") {
			local, exported := fieldText(specifier, "name", file.source), fieldText(specifier, "alias", file.source)
			if exported == "" {
				exported = local
			}
			if exported != name {
				continue
			}
			if source := node.ChildByFieldName("source"); source != nil {
				if target, ok := c.resolver.Resolve(path, stringLiteral(source.Utf8Text(file.source))); ok {
					return c.exported(target, local, seen)
				}
				return nil
			}
			return c.symbol(path, local, local == "default", false)
		}
	}
	if c.ambiguous[key] {
		return nil
	}
	for i := uint(0); i < root.NamedChildCount(); i++ {
		node := root.NamedChild(i)
		if node.Kind() != "export_statement" {
			continue
		}
		source := node.ChildByFieldName("source")
		if source == nil {
			continue
		}
		_, ok := c.resolver.Resolve(path, stringLiteral(source.Utf8Text(file.source)))
		if !ok {
			continue
		}
		specifiers := descendantsOfKind(node, "export_specifier")
		if len(specifiers) == 0 {
			if name == "default" {
				continue
			}
			var match *declaration
			for j := i; j < root.NamedChildCount(); j++ {
				star := root.NamedChild(j)
				if star.Kind() != "export_statement" || star.ChildByFieldName("source") == nil || len(descendantsOfKind(star, "export_specifier")) != 0 {
					continue
				}
				starTarget, ok := c.resolver.Resolve(path, stringLiteral(star.ChildByFieldName("source").Utf8Text(file.source)))
				if !ok {
					continue
				}
				declaration := c.exported(starTarget, name, seen)
				if c.ambiguous[starTarget+"\x00"+name] {
					c.ambiguous[key] = true
					return nil
				}
				if declaration == nil {
					continue
				}
				if match != nil && match.id != declaration.id {
					c.ambiguous[key] = true
					return nil
				}
				match = declaration
			}
			if match != nil {
				return match
			}
			continue
		}
	}
	return nil
}

func newFileOutput(root string, file *syntaxFile, req Request, hashes map[string]string) *fileOutput {
	state := core.FileState{Path: file.path, Kind: "code", Language: file.language, Extension: file.extension, Hash: hashes[file.path], ParserVersion: parserVersion, Generation: req.Generation}
	if full, ok := safeExistingPath(root, file.path); ok {
		if info, err := os.Stat(full); err == nil {
			state.Size, state.MTimeNS = info.Size(), info.ModTime().UnixNano()
		}
	}
	return &fileOutput{indexed: core.IndexedFile{File: state}, units: map[string]bool{}, edges: map[string]bool{}}
}

func (out *fileOutput) addUnit(unit core.CodeUnit) {
	if !out.units[unit.ID] {
		out.units[unit.ID] = true
		out.indexed.Units = append(out.indexed.Units, unit)
	}
}

func (out *fileOutput) addEdge(edge core.CodeEdge) {
	out.addEdgeKey(edge, edgeKey(edge))
}

func (out *fileOutput) addCallEdge(edge core.CodeEdge, node *tree_sitter.Node, lexical string) {
	key := edgeKey(edge) + "\x00" + strconv.FormatUint(uint64(node.StartByte()), 10) + ":" + strconv.FormatUint(uint64(node.EndByte()), 10) + ":" + lexical
	out.addEdgeKey(edge, key)
}

func (out *fileOutput) addEdgeKey(edge core.CodeEdge, key string) {
	if !out.edges[key] {
		out.edges[key] = true
		out.indexed.Edges = append(out.indexed.Edges, edge)
	}
}

func unitFor(file *syntaxFile, req Request, hashes map[string]string, declaration *declaration) core.CodeUnit {
	return core.CodeUnit{
		ID: declaration.id, Name: declaration.name, QualifiedName: declaration.qualified, Kind: declaration.kind,
		Language: file.language, Extension: file.extension, Path: file.path, Source: declaration.source,
		FileHash: hashes[file.path], StartLine: declaration.start, EndLine: declaration.end,
		Generation: req.Generation, Weight: sourceWeight(file.path),
	}
}

func edgeFor(file *syntaxFile, req Request, hashes map[string]string, source, target, relation string, start, end uint32, resolution string) core.CodeEdge {
	return core.CodeEdge{SourceID: source, TargetID: target, Relation: relation, Path: file.path, FileHash: hashes[file.path], Resolution: resolution, StartLine: start, EndLine: end, Generation: req.Generation}
}

func edgeKey(edge core.CodeEdge) string {
	return strings.Join([]string{edge.SourceID, edge.Relation, edge.TargetID, edge.Path, strconv.FormatUint(uint64(edge.StartLine), 10), edge.Resolution}, "\x00")
}

func stableID(path, kind, owner, name string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{path, kind, owner, name}, "\x00")))
	return fmt.Sprintf("js:%x", sum)
}

func moduleID(path string) string { return stableID(path, "file", "", path) }

func externalID(specifier, name string) string {
	if name == "" || name == "*" || name == "default" {
		return "js:external:" + specifier
	}
	return "js:external:" + specifier + "#" + name
}

func externalName(id string) string {
	value := strings.TrimPrefix(id, "js:external:")
	if index := strings.LastIndexByte(value, '#'); index >= 0 {
		return value[index+1:]
	}
	return value
}

func sourcePaths(ctx context.Context, root string, hashes map[string]string, requested []string) ([]string, error) {
	if hashes != nil {
		paths := make([]string, 0, len(hashes))
		for path := range hashes {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if sourceExtension(path) == "" {
				continue
			}
			if _, reason, err := project.InspectWithIndex(root, path, config.Index{IncludeTests: true}); err != nil {
				return nil, err
			} else if reason == "" {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		return paths, nil
	}
	if len(requested) > 0 {
		paths := make([]string, 0, len(requested))
		seen := map[string]bool{}
		for _, path := range requested {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			path, err := safePath(path)
			if err != nil {
				return nil, err
			}
			if seen[path] || sourceExtension(path) == "" {
				continue
			}
			if _, reason, err := project.InspectWithIndex(root, path, config.Index{}); err != nil {
				return nil, err
			} else if reason == "" {
				seen[path] = true
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		return paths, nil
	}
	files, err := project.Discover(ctx, root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if sourceExtension(file.Path) != "" {
			paths = append(paths, file.Path)
		}
	}
	return paths, nil
}

func selectedPaths(root string, requested, available []string, hashes map[string]string) ([]string, error) {
	present := map[string]bool{}
	for _, path := range available {
		present[path] = true
	}
	if len(requested) == 0 {
		if hashes == nil {
			return append([]string(nil), available...), nil
		}
		result := []string{}
		for _, path := range available {
			if _, ok := hashes[path]; ok {
				result = append(result, path)
			}
		}
		return result, nil
	}
	result := []string{}
	seen := map[string]bool{}
	for _, path := range requested {
		clean, err := safePath(path)
		if err != nil {
			return nil, fmt.Errorf("invalid requested path %q: %w", path, err)
		}
		if _, ok := safeExistingPath(root, clean); !ok {
			return nil, fmt.Errorf("requested path %q is missing or escapes project root", path)
		}
		if hashes != nil {
			if _, ok := hashes[clean]; !ok {
				return nil, fmt.Errorf("requested path %q is outside active hash catalog", path)
			}
		}
		if present[clean] && !seen[clean] {
			seen[clean] = true
			result = append(result, clean)
		}
	}
	sort.Strings(result)
	return result, nil
}

func validateHashes(ctx context.Context, root string, values map[string]string) (map[string]string, error) {
	if values == nil {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	originals := make(map[string]map[string]string, len(values))
	for path, hash := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clean, err := safePath(path)
		if err != nil {
			return nil, fmt.Errorf("invalid active path %q: %w", path, err)
		}
		if containsPathPart(clean, "node_modules") {
			return nil, fmt.Errorf("invalid active path %q: node_modules is excluded", path)
		}
		full := filepath.Join(root, filepath.FromSlash(clean))
		if _, err := os.Lstat(full); err == nil {
			if _, ok := safeExistingPath(root, clean); !ok {
				return nil, fmt.Errorf("active path %q escapes project root", path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("validate active path %q: %w", path, err)
		}
		if _, ok := result[clean]; !ok {
			result[clean] = hash
		}
		if originals[clean] == nil {
			originals[clean] = map[string]string{}
		}
		if previous, ok := originals[clean][hash]; !ok || path < previous {
			originals[clean][hash] = path
		}
	}
	conflictPath, first, second := "", "", ""
	for clean, byHash := range originals {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(byHash) < 2 {
			continue
		}
		lowestPath, lowestHash := "", ""
		for hash, path := range byHash {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if lowestPath == "" || path < lowestPath {
				lowestPath, lowestHash = path, hash
			}
		}
		other := ""
		for hash, path := range byHash {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if hash != lowestHash && (other == "" || path < other) {
				other = path
			}
		}
		if conflictPath == "" || clean < conflictPath || clean == conflictPath && (lowestPath < first || lowestPath == first && other < second) {
			conflictPath, first, second = clean, lowestPath, other
		}
	}
	if conflictPath != "" {
		return nil, fmt.Errorf("conflicting active hashes for normalized path %q (%q, %q)", conflictPath, first, second)
	}
	return result, nil
}

func safeExistingPath(root, path string) (string, bool) {
	clean, err := safePath(path)
	if err != nil || containsPathPart(clean, "node_modules") {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		return "", false
	}
	relative, ok := relativeInside(root, resolved)
	if !ok || containsPathPart(relative, "node_modules") {
		return "", false
	}
	return resolved, true
}

func excludedDirectory(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", ".cache", ".next", ".nuxt", "build", "coverage", "dist", "node_modules", "out", "vendor":
		return true
	}
	return false
}

func scriptLanguage(extension string) string {
	if extension == ".ts" || extension == ".tsx" || extension == ".d.ts" {
		return "typescript"
	}
	return "javascript"
}

func sourceWeight(path string) float64 {
	if sourceExtension(path) == ".d.ts" {
		return 0.5
	}
	base := strings.ToLower(filepath.Base(path))
	for _, suffix := range []string{".test.js", ".test.jsx", ".test.ts", ".test.tsx", ".spec.js", ".spec.jsx", ".spec.ts", ".spec.tsx"} {
		if strings.HasSuffix(base, suffix) {
			return 0.6
		}
	}
	if strings.HasPrefix(base, "test_") || containsPathPart(strings.ToLower(path), "test") || containsPathPart(strings.ToLower(path), "tests") || containsPathPart(strings.ToLower(path), "__tests__") {
		return 0.6
	}
	return 1
}

func nodeLines(node *tree_sitter.Node) (uint32, uint32) {
	return uint32(node.StartPosition().Row + 1), uint32(node.EndPosition().Row + 1)
}

func nodeLinesStart(node *tree_sitter.Node) uint32 {
	start, _ := nodeLines(node)
	return start
}

func fileEndLine(source []byte) uint32 {
	if len(source) == 0 {
		return 1
	}
	return uint32(strings.Count(string(source), "\n") + 1)
}

func fieldText(node *tree_sitter.Node, field string, source []byte) string {
	child := node.ChildByFieldName(field)
	if child == nil {
		return ""
	}
	return child.Utf8Text(source)
}

func firstNamedChild(node *tree_sitter.Node) *tree_sitter.Node {
	if node == nil || node.NamedChildCount() == 0 {
		return nil
	}
	return node.NamedChild(0)
}

func walkNamed(node *tree_sitter.Node, visit func(*tree_sitter.Node) bool) {
	if node == nil || !visit(node) {
		return
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		walkNamed(node.NamedChild(i), visit)
	}
}

func descendantsOfKind(node *tree_sitter.Node, kind string) []*tree_sitter.Node {
	result := []*tree_sitter.Node{}
	walkNamed(node, func(current *tree_sitter.Node) bool {
		if current.Kind() == kind {
			result = append(result, current)
		}
		return true
	})
	return result
}

func firstDescendant(node *tree_sitter.Node, kind string) *tree_sitter.Node {
	var result *tree_sitter.Node
	walkNamed(node, func(current *tree_sitter.Node) bool {
		if result != nil {
			return false
		}
		if current.Kind() == kind {
			result = current
			return false
		}
		return true
	})
	return result
}

func ancestor(node *tree_sitter.Node, kind string) *tree_sitter.Node {
	for current := node.Parent(); current != nil; current = current.Parent() {
		if current.Kind() == kind {
			return current
		}
		if current.Kind() == "statement_block" || current.Kind() == "program" {
			return nil
		}
	}
	return nil
}

func isFunctionComponent(name string, node *tree_sitter.Node) bool {
	if name == "" || !unicode.IsUpper([]rune(name)[0]) {
		return false
	}
	found := false
	walkNamed(node, func(child *tree_sitter.Node) bool {
		if child.Kind() == "jsx_element" || child.Kind() == "jsx_self_closing_element" || child.Kind() == "jsx_fragment" {
			found = true
			return false
		}
		return !found
	})
	return found
}

func isClassComponent(node *tree_sitter.Node, source []byte) bool {
	name := fieldText(node, "name", source)
	if name == "" || !unicode.IsUpper([]rune(name)[0]) {
		return false
	}
	heritage := firstDescendant(node, "class_heritage")
	if heritage == nil {
		return false
	}
	text := heritage.Utf8Text(source)
	return strings.Contains(text, "React.Component") || strings.Contains(text, "React.PureComponent") || strings.Contains(text, "Component")
}

func isCallableBoundary(kind string) bool {
	switch kind {
	case "function_declaration", "generator_function_declaration", "function_expression", "generator_function", "arrow_function", "method_definition", "method_signature":
		return true
	}
	return false
}

func baseReference(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '<'); index >= 0 {
		value = value[:index]
	}
	return strings.TrimSpace(value)
}

func stringLiteral(value string) string {
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	if len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '`' && value[len(value)-1] == '`') {
		return value[1 : len(value)-1]
	}
	return ""
}

func isBareSpecifier(specifier string) bool {
	return specifier != "" && !strings.HasPrefix(specifier, ".") && !strings.HasPrefix(specifier, "/")
}

func appendImport(imports []string, ref moduleReference) []string {
	if ref.resolution == "local" || ref.resolution == "external" {
		return append(imports, ref.target)
	}
	return imports
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

func sortedSyntaxPaths(files map[string]*syntaxFile) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func closeSyntaxFiles(files map[string]*syntaxFile) {
	for _, file := range files {
		file.tree.Close()
	}
}
