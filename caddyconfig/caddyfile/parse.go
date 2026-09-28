// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
)

// Parse parses the input just enough to group tokens, in
// order, by server block. No further parsing is performed.
// Server blocks are returned in the order in which they appear.
// Directives that do not appear in validDirectives will cause
// an error. If you do not want to check for valid directives,
// pass in nil instead.
//
// Environment variables in {$ENVIRONMENT_VARIABLE} notation
// will be replaced before parsing begins.
//
// WithImportFileReader (or an ImportFiles value passed through it)
// makes every import directive resolve through the given reader
// instead of through the operating system filesystem: the reader is
// the sole source of imported contents for the call, so replacing
// files on disk between calls can never mix versions into the
// result. Imported paths are normalized lexically (see
// NormalizeImportPath) and glob matches expand in sorted order, so
// the output never depends on the working directory. Parse keeps no
// state between calls: argument expansions, caches, and errors from
// one parse can never be observed by another, even when calls run
// concurrently.
func Parse(filename string, input []byte, opts ...ParseOption) ([]ServerBlock, error) {
	// unfortunately, we must copy the input because parsing must
	// remain a read-only operation, but we have to expand environment
	// variables before we parse, which changes the underlying array (#4422)
	inputCopy := make([]byte, len(input))
	copy(inputCopy, input)

	tokens, err := allTokens(filename, inputCopy)
	if err != nil {
		return nil, err
	}

	cfg := parseConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	p := parser{
		Dispenser: NewDispenser(tokens),
		imports:   openImportSource(cfg.importReader),
		importGraph: importGraph{
			nodes: make(map[string]struct{}),
			edges: make(adjacency),
		},
	}
	blocks, err := p.parseAll()
	if err != nil {
		// never hand back a partially-expanded block list: a
		// failed parse must not be usable as configuration
		return nil, err
	}
	return blocks, nil
}

// allTokens lexes the entire input, but does not parse it.
// It returns all the tokens from the input, unstructured
// and in order. It may mutate input as it expands env vars.
// Unclosed quotes are rejected because the resulting token
// stream would be ambiguous about where arguments end.
func allTokens(filename string, input []byte) ([]Token, error) {
	return tokenizeStrict(replaceEnvVars(input), filename)
}

// replaceEnvVars replaces all occurrences of environment variables.
// It mutates the underlying array and returns the updated slice.
func replaceEnvVars(input []byte) []byte {
	var offset int
	for {
		begin := bytes.Index(input[offset:], spanOpen)
		if begin < 0 {
			break
		}
		begin += offset // make beginning relative to input, not offset
		end := bytes.Index(input[begin+len(spanOpen):], spanClose)
		if end < 0 {
			break
		}
		end += begin + len(spanOpen) // make end relative to input, not begin

		// get the name; if there is no name, skip it
		envString := input[begin+len(spanOpen) : end]
		if len(envString) == 0 {
			offset = end + len(spanClose)
			continue
		}

		// split the string into a key and an optional default
		envParts := strings.SplitN(string(envString), envVarDefaultDelimiter, 2)

		// do a lookup for the env var, replace with the default if not found
		envVarValue, found := os.LookupEnv(envParts[0])
		if !found && len(envParts) == 2 {
			envVarValue = envParts[1]
		}

		// get the value of the environment variable
		// note that this causes one-level deep chaining
		envVarBytes := []byte(envVarValue)

		// splice in the value
		input = append(input[:begin],
			append(envVarBytes, input[end+len(spanClose):]...)...)

		// continue at the end of the replacement
		offset = begin + len(envVarBytes)
	}
	return input
}

type parser struct {
	*Dispenser
	block           ServerBlock // current server block being parsed
	eof             bool        // if we encounter a valid EOF in a hard place
	definedSnippets map[string][]Token
	nesting         int
	imports         importSource
	importGraph     importGraph
}

func (p *parser) parseAll() ([]ServerBlock, error) {
	var blocks []ServerBlock

	for p.Next() {
		err := p.parseOne()
		if err != nil {
			return blocks, err
		}
		if len(p.block.Keys) > 0 || len(p.block.Segments) > 0 {
			blocks = append(blocks, p.block)
		}
		if p.nesting > 0 {
			return blocks, p.EOFErr()
		}
	}

	return blocks, nil
}

func (p *parser) parseOne() error {
	p.block = ServerBlock{}
	return p.begin()
}

func (p *parser) begin() error {
	if len(p.tokens) == 0 {
		return nil
	}

	err := p.addresses()
	if err != nil {
		return err
	}

	if p.eof {
		// this happens if the Caddyfile consists of only
		// a line of addresses and nothing else
		return nil
	}

	if ok, name := p.isNamedRoute(); ok {
		// we just need a dummy leading token to ease parsing later
		nameToken := p.Token()
		nameToken.Text = name

		// named routes only have one key, the route name
		p.block.Keys = []Token{nameToken}
		p.block.IsNamedRoute = true

		// get all the tokens from the block, including the braces
		tokens, err := p.blockTokens(true)
		if err != nil {
			return err
		}
		tokens = append([]Token{nameToken}, tokens...)
		p.block.Segments = []Segment{tokens}
		return nil
	}

	if ok, name := p.isSnippet(); ok {
		if p.definedSnippets == nil {
			p.definedSnippets = map[string][]Token{}
		}
		if _, found := p.definedSnippets[name]; found {
			return p.Errf("redeclaration of previously declared snippet %s", name)
		}
		// consume all tokens til matched close brace
		tokens, err := p.blockTokens(false)
		if err != nil {
			return err
		}
		// Just as we need to track which file the token comes from, we need to
		// keep track of which snippet the token comes from. This is helpful
		// in tracking import cycles across files/snippets by namespacing them.
		// Without this, we end up with false-positives in cycle-detection.
		for k, v := range tokens {
			v.snippetName = name
			tokens[k] = v
		}
		p.definedSnippets[name] = tokens
		// empty block keys so we don't save this block as a real server.
		p.block.Keys = nil
		return nil
	}

	return p.blockContents()
}

func (p *parser) addresses() error {
	var expectingAnother bool

	for {
		value := p.Val()
		token := p.Token()

		// Reject request matchers if trying to define them globally
		if strings.HasPrefix(value, "@") {
			return p.Errf("request matchers may not be defined globally, they must be in a site block; found %s", value)
		}

		// Special case: import directive replaces tokens during parse-time
		if value == "import" && p.isNewLine() {
			err := p.doImport(0)
			if err != nil {
				return err
			}
			continue
		}

		// Open brace definitely indicates end of addresses
		if value == "{" {
			if expectingAnother {
				return p.Errf("Expected another address but had '%s' - check for extra comma", value)
			}
			// Mark this server block as being defined with braces.
			// This is used to provide a better error message when
			// the user may have tried to define two server blocks
			// without having used braces, which are required in
			// that case.
			p.block.HasBraces = true
			break
		}

		// Users commonly forget to place a space between the address and the '{'
		if strings.HasSuffix(value, "{") {
			return p.Errf("Site addresses cannot end with a curly brace: '%s' - put a space between the token and the brace", value)
		}

		if value != "" { // empty token possible if user typed ""
			// Trailing comma indicates another address will follow, which
			// may possibly be on the next line
			if value[len(value)-1] == ',' {
				value = value[:len(value)-1]
				expectingAnother = true
			} else {
				expectingAnother = false // but we may still see another one on this line
			}

			// If there's a comma here, it's probably because they didn't use a space
			// between their two domains, e.g. "foo.com,bar.com", which would not be
			// parsed as two separate site addresses.
			if strings.Contains(value, ",") {
				return p.Errf("Site addresses cannot contain a comma ',': '%s' - put a space after the comma to separate site addresses", value)
			}

			// After the above, a comma surrounded by spaces would result
			// in an empty token which we should ignore
			if value != "" {
				// Add the token as a site address
				token.Text = value
				p.block.Keys = append(p.block.Keys, token)
			}
		}

		// Advance token and possibly break out of loop or return error
		hasNext := p.Next()
		if expectingAnother && !hasNext {
			return p.EOFErr()
		}
		if !hasNext {
			p.eof = true
			break // EOF
		}
		if !expectingAnother && p.isNewLine() {
			break
		}
	}

	return nil
}

func (p *parser) blockContents() error {
	errOpenCurlyBrace := p.openCurlyBrace()
	if errOpenCurlyBrace != nil {
		// single-server configs don't need curly braces
		p.cursor--
	}

	err := p.directives()
	if err != nil {
		return err
	}

	// only look for close curly brace if there was an opening
	if errOpenCurlyBrace == nil {
		err = p.closeCurlyBrace()
		if err != nil {
			return err
		}
	}

	return nil
}

// directives parses through all the lines for directives
// and it expects the next token to be the first
// directive. It goes until EOF or closing curly brace
// which ends the server block.
func (p *parser) directives() error {
	for p.Next() {
		// end of server block
		if p.Val() == "}" {
			// p.nesting has already been decremented
			break
		}

		// special case: import directive replaces tokens during parse-time
		if p.Val() == "import" {
			err := p.doImport(1)
			if err != nil {
				return err
			}
			p.cursor-- // cursor is advanced when we continue, so roll back one more
			continue
		}

		// normal case: parse a directive as a new segment
		// (a "segment" is a line which starts with a directive
		// and which ends at the end of the line or at the end of
		// the block that is opened at the end of the line)
		if err := p.directive(); err != nil {
			return err
		}
	}

	return nil
}

// doImport swaps out the import directive and its argument
// (a total of 2 tokens) with the tokens in the specified file
// or globbing pattern. When the function returns, the cursor
// points at the first imported token, which sits exactly where
// the import directive used to.
func (p *parser) doImport(nesting int) error {
	// remember where the import statement started, so the splice
	// never depends on how many argument or block tokens were read
	importStart := p.cursor

	// syntax checks
	if !p.NextArg() {
		return p.ArgErr()
	}
	importPattern := p.Val()
	if importPattern == "" {
		return p.Err("Import requires a non-empty filepath")
	}

	// grab remaining args as full tokens so that arguments keep
	// their exact bytes and quoting: empty arguments, spaces, and
	// quotes must never be re-tokenized during expansion
	argTokens := p.RemainingArgsAsTokens()

	// grab all the tokens (if it exists) from within a block that follows the import
	var blockTokens []Token
	for currentNesting := p.Nesting(); p.NextBlock(currentNesting); {
		blockTokens = append(blockTokens, p.Token())
	}
	// initialize with size 1
	blockMapping := make(map[string][]Token, 1)
	if len(blockTokens) > 0 {
		// use such tokens to create a new dispenser, and then use it to parse each block
		bd := NewDispenser(blockTokens)

		// one iteration processes one sub-block inside the import
		for bd.Next() {
			currentMappingKey := bd.Val()

			if currentMappingKey == "{" {
				return p.Err("anonymous blocks are not supported")
			}

			// load up all arguments (if there even are any)
			currentMappingTokens := bd.RemainingArgsAsTokens()

			// load up the entire block
			for mappingNesting := bd.Nesting(); bd.NextBlock(mappingNesting); {
				currentMappingTokens = append(currentMappingTokens, bd.Token())
			}

			blockMapping[currentMappingKey] = currentMappingTokens
		}
	}

	// everything before the import statement stays exactly where it was
	tokensBefore := p.tokens[:importStart]
	tokensAfter := p.tokens[p.cursor+1:]
	var importedTokens []Token
	var nodes []string

	// first check snippets. That is a simple, non-recursive replacement
	if p.definedSnippets != nil && p.definedSnippets[importPattern] != nil {
		// deep-copy the stored definition so repeated imports, and the
		// annotations appended below, can never mutate one another or
		// the snippet table (see the same-file, different-branch case)
		stored := p.definedSnippets[importPattern]
		importedTokens = make([]Token, len(stored))
		for i := range stored {
			importedTokens[i] = stored[i].Clone()
		}
		if len(importedTokens) > 0 {
			// just grab the first one
			nodes = append(nodes, fmt.Sprintf("%s:%s", importedTokens[0].File, importedTokens[0].snippetName))
		}
	} else {
		fileTokens, fileNodes, err := p.resolveFileImports(importPattern)
		if err != nil {
			return err
		}
		importedTokens = fileTokens
		nodes = fileNodes
	}

	nodeName := p.File()
	if p.Token().snippetName != "" {
		nodeName += fmt.Sprintf(":%s", p.Token().snippetName)
	}
	p.importGraph.addNode(nodeName)
	p.importGraph.addNodes(nodes)
	if err := p.importGraph.addEdges(nodeName, nodes); err != nil {
		p.importGraph.removeNodes(nodes)
		return err
	}

	tokensCopy := make([]Token, 0, len(importedTokens))

	var (
		maybeSnippet   bool
		maybeSnippetId bool
		index          int
	)

	// Expand argument placeholders for this one layer only: only the
	// arguments given to THIS import statement are visible among these
	// tokens. A nested import reached from within (a named snippet or
	// another file) is expanded later, when the parser reaches the
	// nested statement, with that layer's own arguments in scope, so
	// values passed by an outer layer never leak across layers.
	//
	// golang for range slice return a copy of value
	// similarly, append also copy value
	for i, token := range importedTokens {
		// update the token's imports to refer to import directive filename, line number and snippet name if there is one
		if token.snippetName != "" {
			token.imports = append(token.imports, fmt.Sprintf("%s:%d (import %s)", p.File(), p.Line(), token.snippetName))
		} else {
			token.imports = append(token.imports, fmt.Sprintf("%s:%d (import)", p.File(), p.Line()))
		}

		// naive way of determine snippets, as snippets definition can only follow name + block
		// format, won't check for nesting correctness or any other error, that's what parser does.
		if !maybeSnippet && nesting == 0 {
			// first of the line
			if i == 0 || isNextOnNewLine(tokensCopy[len(tokensCopy)-1], token) {
				index = 0
			} else {
				index++
			}

			if index == 0 && len(token.Text) >= 3 && strings.HasPrefix(token.Text, "(") && strings.HasSuffix(token.Text, ")") {
				maybeSnippetId = true
			}
		}

		switch token.Text {
		case "{":
			nesting++
			if index == 1 && maybeSnippetId && nesting == 1 {
				maybeSnippet = true
				maybeSnippetId = false
			}
		case "}":
			nesting--
			if nesting == 0 && maybeSnippet {
				maybeSnippet = false
			}
		}
		// if it is {block}, we substitute with all tokens in the block
		// if it is {blocks.*}, we substitute with the tokens in the mapping for the *
		var blockTokensToAdd []Token
		foundBlockDirective := false
		switch {
		case token.Text == "{block}":
			foundBlockDirective = true
			blockTokensToAdd = blockTokens
		case strings.HasPrefix(token.Text, "{blocks.") && strings.HasSuffix(token.Text, "}"):
			foundBlockDirective = true
			// {blocks.foo.bar} will be extracted to key `foo.bar`
			blockKey := strings.TrimPrefix(strings.TrimSuffix(token.Text, "}"), "{blocks.")
			val, ok := blockMapping[blockKey]
			if ok {
				blockTokensToAdd = val
			}
		}

		if foundBlockDirective {
			if maybeSnippet {
				tokensCopy = append(tokensCopy, token)
			} else {
				tokensCopy = append(tokensCopy, blockTokensToAdd...)
			}
			continue
		}

		if maybeSnippet {
			// placeholders inside a snippet definition are not
			// expanded until the snippet itself is imported
			tokensCopy = append(tokensCopy, token)
			continue
		}

		replaced, err := substituteImportArgs(token, argTokens)
		if err != nil {
			return err
		}
		tokensCopy = append(tokensCopy, replaced...)
	}

	// splice the imported tokens in the place of the import statement;
	// the cursor lands on the first imported token so parsing simply
	// continues into the expansion
	p.tokens = append(tokensBefore, append(tokensCopy, tokensAfter...)...)
	p.cursor = importStart

	return nil
}

// resolveFileImports turns the import pattern into the tokens of
// every matching file, reading from the configured reader when one
// was given or from disk otherwise.
func (p *parser) resolveFileImports(importPattern string) ([]Token, []string, error) {
	// parsers built directly (for example in tests) do not go
	// through Parse, so they have no source yet; disk is the default
	if p.imports == nil {
		p.imports = diskSource{}
	}
	if _, isDisk := p.imports.(diskSource); !isDisk {
		return p.resolveVirtualImports(importPattern)
	}
	return p.resolveDiskImports(importPattern)
}

// resolveVirtualImports resolves an import pattern entirely through
// the configured ImportFileReader. Relative patterns are joined with
// the normalized directory of the importing file, cleaning "." and
// ".." lexically, and glob matches expand in sorted order.
func (p *parser) resolveVirtualImports(importPattern string) ([]Token, []string, error) {
	importingFile := normalizeImportPath(p.Dispenser.File())
	globPattern, err := resolveImportPath(importingFile, importPattern)
	if err != nil {
		return nil, nil, p.Errf("%v", err)
	}

	hasMeta := strings.ContainsAny(globPattern, "*?[")
	var matches []string
	if hasMeta {
		// bound pathological patterns just like the on-disk path
		// (issue #2096)
		if strings.Count(globPattern, "*") > 1 || strings.Count(globPattern, "?") > 1 ||
			(strings.Contains(globPattern, "[") && strings.Contains(globPattern, "]")) {
			return nil, nil, p.Errf("Glob pattern may only contain one wildcard (*), but has others: %s", globPattern)
		}
		matches, err = p.imports.globPattern(globPattern)
		if err != nil {
			return nil, nil, p.Errf("Failed to use import pattern %s: %v", importPattern, err)
		}
	} else {
		matches = []string{globPattern}
	}

	if len(matches) == 0 {
		if hasMeta {
			caddy.Log().Warn("No files matching import glob pattern", zap.String("pattern", importPattern))
			return nil, nil, nil
		}
		return nil, nil, p.Errf("File to import not found: %s", importPattern)
	}
	matches = skipDotfiles(globPattern, matches)

	var importedTokens []Token
	for _, importFile := range matches {
		newTokens, err := p.readImportFile(importFile)
		if err != nil {
			return nil, nil, err
		}
		importedTokens = append(importedTokens, newTokens...)
	}
	return importedTokens, matches, nil
}

// resolveDiskImports preserves the on-disk behavior: patterns are
// resolved relative to the importing token's file (issue #867),
// globs are bounded against pathological expansion (issue #2096),
// and dotfiles are skipped for directory-star globs (issue #5295).
func (p *parser) resolveDiskImports(importPattern string) ([]Token, []string, error) {
	// make path relative to the file of the _token_ being processed rather
	// than current working directory (issue #867) and then use glob to get
	// list of matching filenames
	absFile, err := caddy.FastAbs(p.Dispenser.File())
	if err != nil {
		return nil, nil, p.Errf("Failed to get absolute path of file: %s: %v", p.Dispenser.File(), err)
	}

	var matches []string
	var globPattern string
	if !filepath.IsAbs(importPattern) {
		globPattern = filepath.Join(filepath.Dir(absFile), importPattern)
	} else {
		globPattern = importPattern
	}
	if strings.Count(globPattern, "*") > 1 || strings.Count(globPattern, "?") > 1 ||
		(strings.Contains(globPattern, "[") && strings.Contains(globPattern, "]")) {
		// See issue #2096 - a pattern with many glob expansions can hang for too long
		return nil, nil, p.Errf("Glob pattern may only contain one wildcard (*), but has others: %s", globPattern)
	}
	matches, err = p.imports.globPattern(globPattern)
	if err != nil {
		return nil, nil, p.Errf("Failed to use import pattern %s: %v", importPattern, err)
	}
	if len(matches) == 0 {
		if strings.ContainsAny(globPattern, "*?[]") {
			caddy.Log().Warn("No files matching import glob pattern", zap.String("pattern", importPattern))
			return nil, nil, nil
		}
		return nil, nil, p.Errf("File to import not found: %s", importPattern)
	}
	matches = skipDotfiles(globPattern, matches)

	var importedTokens []Token
	for _, importFile := range matches {
		newTokens, err := p.readImportFile(importFile)
		if err != nil {
			return nil, nil, err
		}
		importedTokens = append(importedTokens, newTokens...)
	}
	return importedTokens, matches, nil
}

// skipDotfiles drops dotfile matches when the final pattern segment
// is a bare star, mirroring the historical glob behavior.
func skipDotfiles(globPattern string, matches []string) []string {
	sep := "/"
	if strings.Contains(globPattern, string(filepath.Separator)) {
		sep = string(filepath.Separator)
	}
	segGlobPattern := strings.Split(globPattern, sep)
	if !strings.HasPrefix(segGlobPattern[len(segGlobPattern)-1], "*") {
		return matches
	}
	var filtered []string
	for _, m := range matches {
		seg := strings.Split(m, sep)
		if strings.HasPrefix(seg[len(seg)-1], ".") {
			continue
		}
		filtered = append(filtered, m)
	}
	return filtered
}

// readImportFile lexes the individual file identified by importFile
// through the configured source and returns its tokens. Token file
// names are normalized to stable paths: the absolute on-disk path
// when reading disk (issue #1892), and the normalized virtual path
// through an ImportFileReader, neither of which depends on the
// working directory.
func (p *parser) readImportFile(importFile string) ([]Token, error) {
	input, err := p.imports.readFile(importFile)
	if err != nil {
		if errors.Is(err, errImportIsDir) {
			return nil, p.Errf("Could not import %s: is a directory", importFile)
		}
		// wrap with %w so failures from a custom ImportFileReader
		// remain inspectable with errors.Is/errors.As
		return nil, p.WrapErr(fmt.Errorf("Could not import %s: %w", importFile, err))
	}

	// only warning in case of empty files
	if len(input) == 0 || len(strings.TrimSpace(string(input))) == 0 {
		caddy.Log().Warn("Import file is empty", zap.String("file", importFile))
		return []Token{}, nil
	}

	importedTokens, err := allTokens(importFile, input)
	if err != nil {
		return nil, p.Errf("Could not read tokens while importing %s: %v", importFile, err)
	}

	filename := importFile
	if _, isDisk := p.imports.(diskSource); isDisk {
		// Tack the absolute path onto these tokens on disk so errors
		// show the imported file's name reliably (issue #1892)
		absName, absErr := caddy.FastAbs(importFile)
		if absErr != nil {
			return nil, p.Errf("Failed to get absolute path of file: %s: %v", importFile, absErr)
		}
		filename = absName
	}
	for i := range importedTokens {
		importedTokens[i].File = filename
	}

	return importedTokens, nil
}

// directive collects tokens until the directive's scope
// closes (either end of line or end of curly brace block).
// It expects the currently-loaded token to be a directive
// (or } that ends a server block). The collected tokens
// are loaded into the current server block for later use
// by directive setup functions.
func (p *parser) directive() error {
	// a segment is a list of tokens associated with this directive
	var segment Segment

	// the directive itself is appended as a relevant token
	segment = append(segment, p.Token())

	for p.Next() {
		if p.Val() == "{" {
			p.nesting++
			if !p.isNextOnNewLine() && p.Token().wasQuoted == 0 {
				return p.Err("Unexpected next token after '{' on same line")
			}
			if p.isNewLine() {
				return p.Err("Unexpected '{' on a new line; did you mean to place the '{' on the previous line?")
			}
		} else if p.Val() == "{}" {
			if p.isNextOnNewLine() && p.Token().wasQuoted == 0 {
				return p.Err("Unexpected '{}' at end of line")
			}
		} else if p.isNewLine() && p.nesting == 0 {
			p.cursor-- // read too far
			break
		} else if p.Val() == "}" && p.nesting > 0 {
			p.nesting--
		} else if p.Val() == "}" && p.nesting == 0 {
			return p.Err("Unexpected '}' because no matching opening brace")
		} else if p.Val() == "import" && p.isNewLine() {
			if err := p.doImport(1); err != nil {
				return err
			}
			p.cursor-- // cursor is advanced when we continue, so roll back one more
			continue
		}

		segment = append(segment, p.Token())
	}

	p.block.Segments = append(p.block.Segments, segment)

	if p.nesting > 0 {
		return p.EOFErr()
	}

	return nil
}

// openCurlyBrace expects the current token to be an
// opening curly brace. This acts like an assertion
// because it returns an error if the token is not
// an opening curly brace. It does NOT advance the token.
func (p *parser) openCurlyBrace() error {
	if p.Val() != "{" {
		if p.valLooksLikeGlobalOptionsAfterImportedSnippets() {
			return p.Err("global options block must appear before import directives; move the global options block to the top of the Caddyfile")
		}
		return p.SyntaxErr("{")
	}
	return nil
}

func (p *parser) valLooksLikeGlobalOptionsAfterImportedSnippets() bool {
	if p.Val() != "import" || len(p.block.Keys) == 0 {
		return false
	}

	for _, key := range p.block.Keys {
		if !strings.HasPrefix(key.Text, "(") || !strings.HasSuffix(key.Text, ")") {
			return false
		}
	}

	return true
}

// closeCurlyBrace expects the current token to be
// a closing curly brace. This acts like an assertion
// because it returns an error if the token is not
// a closing curly brace. It does NOT advance the token.
func (p *parser) closeCurlyBrace() error {
	if p.Val() != "}" {
		return p.SyntaxErr("}")
	}
	return nil
}

func (p *parser) isNamedRoute() (bool, string) {
	keys := p.block.Keys
	// A named route block is a single key with parens, prefixed with &.
	if len(keys) == 1 && strings.HasPrefix(keys[0].Text, "&(") && strings.HasSuffix(keys[0].Text, ")") {
		return true, strings.TrimSuffix(keys[0].Text[2:], ")")
	}
	return false, ""
}

func (p *parser) isSnippet() (bool, string) {
	keys := p.block.Keys
	// A snippet block is a single key with parens. Nothing else qualifies.
	if len(keys) == 1 && strings.HasPrefix(keys[0].Text, "(") && strings.HasSuffix(keys[0].Text, ")") {
		return true, strings.TrimSuffix(keys[0].Text[1:], ")")
	}
	return false, ""
}

// read and store everything in a block for later replay.
func (p *parser) blockTokens(retainCurlies bool) ([]Token, error) {
	// block must have curlies.
	err := p.openCurlyBrace()
	if err != nil {
		return nil, err
	}
	nesting := 1 // count our own nesting
	tokens := []Token{}
	if retainCurlies {
		tokens = append(tokens, p.Token())
	}
	for p.Next() {
		if p.Val() == "}" {
			nesting--
			if nesting == 0 {
				if retainCurlies {
					tokens = append(tokens, p.Token())
				}
				break
			}
		}
		if p.Val() == "{" {
			nesting++
		}
		tokens = append(tokens, p.tokens[p.cursor])
	}
	// make sure we're matched up
	if nesting != 0 {
		return nil, p.SyntaxErr("}")
	}
	return tokens, nil
}

// ServerBlock associates any number of keys from the
// head of the server block with tokens, which are
// grouped by segments.
type ServerBlock struct {
	HasBraces    bool
	Keys         []Token
	Segments     []Segment
	IsNamedRoute bool
}

func (sb ServerBlock) GetKeysText() []string {
	res := make([]string, 0, len(sb.Keys))
	for _, k := range sb.Keys {
		res = append(res, k.Text)
	}
	return res
}

// DispenseDirective returns a dispenser that contains
// all the tokens in the server block.
func (sb ServerBlock) DispenseDirective(dir string) *Dispenser {
	var tokens []Token
	for _, seg := range sb.Segments {
		if len(seg) > 0 && seg[0].Text == dir {
			tokens = append(tokens, seg...)
		}
	}
	return NewDispenser(tokens)
}

// Segment is a list of tokens which begins with a directive
// and ends at the end of the directive (either at the end of
// the line, or at the end of a block it opens).
type Segment []Token

// Directive returns the directive name for the segment.
// The directive name is the text of the first token.
func (s Segment) Directive() string {
	if len(s) > 0 {
		return s[0].Text
	}
	return ""
}

// spanOpen and spanClose are used to bound spans that
// contain the name of an environment variable.
var (
	spanOpen, spanClose    = []byte{'{', '$'}, []byte{'}'}
	envVarDefaultDelimiter = ":"
)
