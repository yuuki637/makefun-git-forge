package main

import (
	"errors"
	"strconv"
	"strings"
)

// GraphQL surface (ported from graphql.ts): tokenizer, parser, executor
// covering the fixed document set the conformance suite sends.

// ------------------------------------------------------------------ values
type gValue struct {
	kind   string // var|string|int|float|boolean|enum|null|list|object
	name   string
	str    string
	num    float64
	isInt  bool
	b      bool
	items  []*gValue
	fields map[string]*gValue
	order  []string
}

type fieldSel struct {
	name         string
	alias        string
	hasAlias     bool
	args         map[string]*gValue
	argOrder     []string
	selectionSet []gSel
}

type gSel struct {
	kind          string // field | spread | inline
	field         *fieldSel
	name          string // spread name
	typeCondition string
	selectionSet  []gSel
}

type gOperation struct {
	typ          string // query | mutation
	name         string
	varDefaults  map[string]*gValue
	selectionSet []gSel
}

type fragmentDef struct {
	typeCondition string
	selectionSet  []gSel
}

// ---------------------------------------------------------------- tokenizer
type gToken struct {
	kind  string // name|string|number|punct|spread|eof
	value string
}

func tokenizeGraphql(input string) ([]gToken, error) {
	var tokens []gToken
	i := 0
	for i < len(input) {
		ch := input[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' || ch == 0xef || ch == 0xbb || ch == 0xbf {
			i++
			continue
		}
		if ch == '#' {
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && i+3 <= len(input) && input[i:i+3] == "..." {
			tokens = append(tokens, gToken{"spread", "..."})
			i += 3
			continue
		}
		if strings.IndexByte("!$():=@[]{}|&", ch) >= 0 {
			tokens = append(tokens, gToken{"punct", string(ch)})
			i++
			continue
		}
		if ch == '"' {
			if i+3 <= len(input) && input[i:i+3] == `"""` {
				end := strings.Index(input[i+3:], `"""`)
				if end == -1 {
					tokens = append(tokens, gToken{"string", input[i+3:]})
					i = len(input)
				} else {
					tokens = append(tokens, gToken{"string", input[i+3 : i+3+end]})
					i = i + 3 + end + 3
				}
				continue
			}
			j := i + 1
			var out strings.Builder
			for j < len(input) && input[j] != '"' {
				if input[j] == '\\' && j+1 < len(input) {
					esc := input[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					case 'b':
						out.WriteByte('\b')
					case 'f':
						out.WriteByte('\f')
					case 'u':
						if j+6 <= len(input) {
							code, _ := strconv.ParseInt(input[j+2:j+6], 16, 32)
							out.WriteRune(rune(code))
							j += 4
						}
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(input[j])
					j++
				}
			}
			tokens = append(tokens, gToken{"string", out.String()})
			i = j + 1
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			j := i
			for j < len(input) && strings.IndexByte("0123456789.eE+-", input[j]) >= 0 {
				j++
			}
			tokens = append(tokens, gToken{"number", input[i:j]})
			i = j
			continue
		}
		if ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') {
			j := i
			for j < len(input) && (input[j] == '_' || (input[j] >= '0' && input[j] <= '9') || (input[j] >= 'A' && input[j] <= 'Z') || (input[j] >= 'a' && input[j] <= 'z')) {
				j++
			}
			tokens = append(tokens, gToken{"name", input[i:j]})
			i = j
			continue
		}
		return nil, errors.New("unexpected character " + strconv.Quote(string(ch)))
	}
	tokens = append(tokens, gToken{"eof", ""})
	return tokens, nil
}

// ------------------------------------------------------------------- parser
type gParser struct {
	tokens []gToken
	pos    int
}

func (p *gParser) peek() gToken  { return p.tokens[p.pos] }
func (p *gParser) next() gToken  { t := p.tokens[p.pos]; p.pos++; return t }
func (p *gParser) expectPunct(v string) error {
	tok := p.next()
	if tok.kind != "punct" || tok.value != v {
		return errors.New("expected " + v + ", got " + tok.value)
	}
	return nil
}
func (p *gParser) skipPunct(v string) bool {
	tok := p.peek()
	if tok.kind == "punct" && tok.value == v {
		p.pos++
		return true
	}
	return false
}
func (p *gParser) expectName() (string, error) {
	tok := p.next()
	if tok.kind != "name" {
		return "", errors.New("expected name, got " + tok.value)
	}
	return tok.value, nil
}

func (p *gParser) parseValue() (*gValue, error) {
	tok := p.next()
	if tok.kind == "punct" && tok.value == "$" {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gValue{kind: "var", name: name}, nil
	}
	if tok.kind == "string" {
		return &gValue{kind: "string", str: tok.value}, nil
	}
	if tok.kind == "number" {
		f, _ := strconv.ParseFloat(tok.value, 64)
		isInt := !(strings.ContainsAny(tok.value, ".eE"))
		return &gValue{kind: "int", num: f, isInt: isInt}, nil
	}
	if tok.kind == "name" {
		switch tok.value {
		case "true":
			return &gValue{kind: "boolean", b: true}, nil
		case "false":
			return &gValue{kind: "boolean", b: false}, nil
		case "null":
			return &gValue{kind: "null"}, nil
		default:
			return &gValue{kind: "enum", str: tok.value}, nil
		}
	}
	if tok.kind == "punct" && tok.value == "[" {
		v := &gValue{kind: "list"}
		for !p.skipPunct("]") {
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.items = append(v.items, item)
		}
		return v, nil
	}
	if tok.kind == "punct" && tok.value == "{" {
		v := &gValue{kind: "object", fields: map[string]*gValue{}}
		for !p.skipPunct("}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			if _, dup := v.fields[name]; !dup {
				v.order = append(v.order, name)
			}
			v.fields[name] = val
		}
		return v, nil
	}
	return nil, errors.New("unexpected value token " + tok.value)
}

func (p *gParser) parseSelectionSet() ([]gSel, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []gSel
	for !p.skipPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
	return selections, nil
}

func (p *gParser) parseSelection() (gSel, error) {
	tok := p.peek()
	if tok.kind == "spread" {
		p.next()
		nameTok := p.peek()
		if nameTok.kind == "name" && nameTok.value == "on" {
			p.next()
			typeCondition, err := p.expectName()
			if err != nil {
				return gSel{}, err
			}
			p.skipDirectives()
			selectionSet, err := p.parseSelectionSet()
			if err != nil {
				return gSel{}, err
			}
			return gSel{kind: "inline", typeCondition: typeCondition, selectionSet: selectionSet}, nil
		}
		name, err := p.expectName()
		if err != nil {
			return gSel{}, err
		}
		p.skipDirectives()
		return gSel{kind: "spread", name: name}, nil
	}
	name, err := p.expectName()
	if err != nil {
		return gSel{}, err
	}
	field := &fieldSel{name: name, args: map[string]*gValue{}}
	if p.skipPunct(":") {
		field.alias = name
		field.hasAlias = true
		field.name, err = p.expectName()
		if err != nil {
			return gSel{}, err
		}
	}
	if p.skipPunct("(") {
		for !p.skipPunct(")") {
			argName, err := p.expectName()
			if err != nil {
				return gSel{}, err
			}
			if err := p.expectPunct(":"); err != nil {
				return gSel{}, err
			}
			val, err := p.parseValue()
			if err != nil {
				return gSel{}, err
			}
			if _, dup := field.args[argName]; !dup {
				field.argOrder = append(field.argOrder, argName)
			}
			field.args[argName] = val
		}
	}
	p.skipDirectives()
	if p.peek().kind == "punct" && p.peek().value == "{" {
		selectionSet, err := p.parseSelectionSet()
		if err != nil {
			return gSel{}, err
		}
		field.selectionSet = selectionSet
	}
	return gSel{kind: "field", field: field}, nil
}

func (p *gParser) skipDirectives() {
	for p.peek().kind == "punct" && p.peek().value == "@" {
		p.next()
		p.expectName()
		if p.skipPunct("(") {
			depth := 1
			for depth > 0 {
				tok := p.next()
				if tok.kind == "eof" {
					break
				}
				if tok.kind == "punct" && tok.value == "(" {
					depth++
				}
				if tok.kind == "punct" && tok.value == ")" {
					depth--
				}
			}
		}
	}
}

func (p *gParser) parseType() error {
	if p.skipPunct("[") {
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	p.skipPunct("!")
	return nil
}

func (p *gParser) parseDocument() ([]*gOperation, map[string]*fragmentDef, error) {
	var operations []*gOperation
	fragments := map[string]*fragmentDef{}
	for p.peek().kind != "eof" {
		tok := p.peek()
		if tok.kind == "punct" && tok.value == "{" {
			selectionSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, &gOperation{typ: "query", varDefaults: map[string]*gValue{}, selectionSet: selectionSet})
			continue
		}
		keyword, err := p.expectName()
		if err != nil {
			return nil, nil, err
		}
		if keyword == "query" || keyword == "mutation" || keyword == "subscription" {
			op := &gOperation{typ: "query", varDefaults: map[string]*gValue{}}
			if keyword == "mutation" {
				op.typ = "mutation"
			}
			if p.peek().kind == "name" {
				op.name, _ = p.expectName()
			}
			if p.skipPunct("(") {
				for !p.skipPunct(")") {
					if err := p.expectPunct("$"); err != nil {
						return nil, nil, err
					}
					varName, err := p.expectName()
					if err != nil {
						return nil, nil, err
					}
					if err := p.expectPunct(":"); err != nil {
						return nil, nil, err
					}
					if err := p.parseType(); err != nil {
						return nil, nil, err
					}
					if p.skipPunct("=") {
						val, err := p.parseValue()
						if err != nil {
							return nil, nil, err
						}
						op.varDefaults[varName] = val
					}
					p.skipDirectives()
				}
			}
			p.skipDirectives()
			selectionSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			op.selectionSet = selectionSet
			operations = append(operations, op)
			continue
		}
		if keyword == "fragment" {
			fragName, err := p.expectName()
			if err != nil {
				return nil, nil, err
			}
			onTok, err := p.expectName()
			if err != nil {
				return nil, nil, err
			}
			if onTok != "on" {
				return nil, nil, errors.New("expected on in fragment definition")
			}
			typeCondition, err := p.expectName()
			if err != nil {
				return nil, nil, err
			}
			p.skipDirectives()
			selectionSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			fragments[fragName] = &fragmentDef{typeCondition: typeCondition, selectionSet: selectionSet}
			continue
		}
		return nil, nil, errors.New("unexpected definition " + keyword)
	}
	return operations, fragments, nil
}

func evalValue(value *gValue, vars map[string]interface{}, defaults map[string]*gValue) interface{} {
	switch value.kind {
	case "var":
		if v, ok := vars[value.name]; ok {
			return v
		}
		if d, ok := defaults[value.name]; ok {
			return evalValue(d, vars, defaults)
		}
		return nil
	case "string", "enum":
		return value.str
	case "int":
		if value.isInt {
			return int64(value.num)
		}
		return value.num
	case "float":
		return value.num
	case "boolean":
		return value.b
	case "null":
		return nil
	case "list":
		out := make([]interface{}, len(value.items))
		for i, item := range value.items {
			out[i] = evalValue(item, vars, defaults)
		}
		return out
	case "object":
		out := map[string]interface{}{}
		for _, key := range value.order {
			out[key] = evalValue(value.fields[key], vars, defaults)
		}
		return out
	}
	return nil
}
