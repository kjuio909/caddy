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
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
)

// doImport swaps out the import directive and its argument
// (a total of 2 tokens) with the tokens in the specified file
// or globbing pattern. When the function returns, the cursor
// is on the token before where the import directive was. In
// other words, call Next() to access the first token that was
// imported.
func (p *parser) doImport(nesting int) error {
	// syntax checks
	if !p.NextArg() {
		return p.ArgErr()
	}
	importPattern := p.Val()
	if importPattern == "" {
		return p.Err("Import requires a non-empty filepath")
	}

	// grab remaining args as placeholder replacements; empty args and
	// args containing whitespace or quotes are each a single raw token
	args := p.RemainingArgs()

	// the import keyword token anchors every error raised by this expansion
	importToken := p.Token()

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

	// splice out the import directive and its arguments
	// (2 tokens, plus the length of args)
	tokensBefore := p.tokens[:p.cursor-1-len(args)-len(blockTokens)]
	tokensAfter := p.tokens[p.cursor+1:]

	var importedTokens []Token

	// first check snippets. That is a simple, non-recursive replacement
	if p.definedSnippets != nil && p.definedSnippets[importPattern] != nil {
		sourceTokens := p.definedSnippets[importPattern]
		key := ""
		if len(sourceTokens) > 0 {
			key = snippetNodeKey(sourceTokens[0])
		}
		if err := checkActiveImport(importToken, key); err != nil {
			return err
		}
		tokens, err := p.expandImportedTokens(importToken, sourceTokens, key, args, blockTokens, blockMapping, nesting)
		if err != nil {
			return err
		}
		importedTokens = append(importedTokens, tokens...)
	} else {
		// make path relative to the file of the _token_ being processed rather
		// than current working directory (issue #867) and then use glob to get
		// list of matching filenames
		absFile, err := caddy.FastAbs(p.Dispenser.File())
		if err != nil {
			return p.Errf("Failed to get absolute path of file: %s: %v", p.Dispenser.File(), err)
		}
		absFile = filepath.Clean(absFile)

		var globPattern string
		if !filepath.IsAbs(importPattern) {
			globPattern = filepath.Join(filepath.Dir(absFile), importPattern)
		} else {
			globPattern = importPattern
		}
		// clean "."/".." segments based on the declaring file's directory
		globPattern = filepath.Clean(globPattern)

		if strings.Count(globPattern, "*") > 1 || strings.Count(globPattern, "?") > 1 ||
			(strings.Contains(globPattern, "[") && strings.Contains(globPattern, "]")) {
			// See issue #2096 - a pattern with many glob expansions can hang for too long
			return p.Errf("Glob pattern may only contain one wildcard (*), but has others: %s", globPattern)
		}
		matches, err := p.snapshot.glob(globPattern)
		if err != nil {
			return p.Errf("Failed to use import pattern %s: %v", importPattern, err)
		}
		if len(matches) == 0 {
			if strings.ContainsAny(globPattern, "*?[]") {
				// a glob matching nothing keeps the established successful,
				// no-op semantics; do not manufacture an error
				caddy.Log().Warn("No files matching import glob pattern", zap.String("pattern", globPattern))
			} else {
				// report the normalized path so the message does not
				// depend on the working directory
				return p.Errf("File to import not found: %s", globPattern)
			}
		} else if strings.HasPrefix(filepath.Base(globPattern), "*") {
			// See issue #5295 - skip files that start with a . when the
			// final pattern segment begins with a wildcard. Filter on a
			// copy so the pinned snapshot entry stays intact.
			filtered := make([]string, 0, len(matches))
			for _, m := range matches {
				if !strings.HasPrefix(filepath.Base(m), ".") {
					filtered = append(filtered, m)
				}
			}
			matches = filtered
		}

		// collect all the imported tokens; matches are normalized and in
		// lexicographic order, and every referenced file is expanded on
		// its own even when another statement already imported it
		for _, importFile := range matches {
			sourceTokens, err := p.doSingleImport(importFile)
			if err != nil {
				return err
			}
			if err := checkActiveImport(importToken, importFile); err != nil {
				return err
			}
			tokens, err := p.expandImportedTokens(importToken, sourceTokens, importFile, args, blockTokens, blockMapping, nesting)
			if err != nil {
				return err
			}
			importedTokens = append(importedTokens, tokens...)
		}
	}

	// splice the imported tokens in the place of the import statement
	// and rewind cursor so Next() will land on first imported token
	p.tokens = append(tokensBefore, append(importedTokens, tokensAfter...)...)
	p.cursor -= len(args) + len(blockTokens) + 1

	return nil
}

// snippetNodeKey is the machine-readable key of a snippet for cycle
// detection, namespaced by the file that defines it.
func snippetNodeKey(t Token) string {
	if t.snippetName != "" {
		return fmt.Sprintf("%s:%s", t.File, t.snippetName)
	}
	return ""
}

// checkActiveImport fails if node already appears in the active import
// chain of the token triggering the expansion, reporting the first
// failing position together with a readable recursion chain.
func checkActiveImport(trigger Token, node string) error {
	if node == "" {
		return nil
	}
	if slices.Contains(trigger.importChain, node) {
		chain := make([]string, 0, len(trigger.importChain)+1)
		chain = append(chain, trigger.importChain...)
		chain = append(chain, node)
		return tokenErrf(trigger, "import cycle detected: %s", strings.Join(chain, " -> "))
	}
	return nil
}

// expandImportedTokens annotates sourceTokens with this expansion level
// and applies block and argument substitution, returning the tokens to
// splice in. nodeKey is the normalized file path or snippet key the
// tokens originate from. The machine-readable recursion chain is
// inherited from trigger, the import statement token in the enclosing
// layer, so nested imports accumulate through the spliced tokens.
func (p *parser) expandImportedTokens(trigger Token, sourceTokens []Token, nodeKey string, args []string,
	blockTokens []Token, blockMapping map[string][]Token, nesting int) ([]Token, error) {
	// copy the tokens so we don't overwrite p.definedSnippets
	tokensCopy := make([]Token, 0, len(sourceTokens))

	// active chain of the enclosing layer; this expansion appends nodeKey
	baseChain := append([]string{}, trigger.importChain...)
	if nodeKey != "" && !slices.Contains(baseChain, nodeKey) {
		baseChain = append(baseChain, nodeKey)
	}

	var (
		maybeSnippet   bool
		maybeSnippetId bool
		index          int
	)

	// golang for range gives a copy of the value; append copies it too
	for i, token := range sourceTokens {
		// update the token's imports to refer to the import directive
		// filename and line number, and the snippet name when there is one
		humanImport := fmt.Sprintf("%s:%d (import)", p.File(), p.Line())
		if token.snippetName != "" {
			humanImport = fmt.Sprintf("%s:%d (import %s)", p.File(), p.Line(), token.snippetName)
		}
		token.imports = append(append([]string{}, token.imports...), humanImport)

		// machine-readable active recursion chain for this expansion level
		token.importChain = append([]string{}, baseChain...)

		// naive way of determining snippets, as snippet definitions can
		// only follow a name + block format; nesting correctness or any
		// other error is not checked here, that's what the parser does.
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
		// if it is {block}, substitute with all tokens in the block;
		// if it is {blocks.*}, substitute with the mapping for the *
		var tokensToAdd []Token
		foundBlockDirective := false
		switch {
		case token.Text == "{block}":
			foundBlockDirective = true
			tokensToAdd = blockTokens
		case strings.HasPrefix(token.Text, "{blocks.") && strings.HasSuffix(token.Text, "}"):
			foundBlockDirective = true
			// {blocks.foo.bar} extracts the key `foo.bar`
			blockKey := strings.TrimPrefix(strings.TrimSuffix(token.Text, "}"), "{blocks.")
			tokensToAdd = blockMapping[blockKey]
		}

		if foundBlockDirective {
			if maybeSnippet {
				tokensCopy = append(tokensCopy, token)
			} else {
				tokensCopy = append(tokensCopy, tokensToAdd...)
			}
			continue
		}

		if maybeSnippet {
			// tokens inside a snippet definition are a template: this
			// import's arguments must not leak into them; placeholders
			// are resolved from the snippet's own invocation layer later
			tokensCopy = append(tokensCopy, token)
			continue
		}

		replaced, err := substituteArgsToken(token, args)
		if err != nil {
			return nil, err
		}
		tokensCopy = append(tokensCopy, replaced...)
	}

	return tokensCopy, nil
}

// doSingleImport lexes the individual file at importFile using the
// per-call snapshot and returns its tokens or an error, if any.
func (p *parser) doSingleImport(importFile string) ([]Token, error) {
	importFile = filepath.Clean(importFile)

	input, err := p.snapshot.read(importFile)
	if err != nil {
		return nil, p.WrapErr(fmt.Errorf("could not import %s: %v", importFile, err))
	}

	// only warning in case of empty files
	if len(input) == 0 || len(strings.TrimSpace(string(input))) == 0 {
		caddy.Log().Warn("Import file is empty", zap.String("file", importFile))
		return []Token{}, nil
	}

	importedTokens, err := allTokens(importFile, input)
	if err != nil {
		return nil, p.WrapErr(fmt.Errorf("could not read tokens while importing %s: %v", importFile, err))
	}

	// tack the normalized, absolute path onto these tokens so relative
	// imports resolve from this file and errors show its name (#1892)
	for i := range importedTokens {
		importedTokens[i].File = importFile
	}

	return importedTokens, nil
}
