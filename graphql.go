package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type GqlValue struct {
	K      string // 'var', 'str', 'num', 'bool', 'null', 'list', 'obj'
	Name   string // for 'var' or enum / unquoted literal
	V      any    // for 'str', 'num', 'bool'
	Items  []*GqlValue
	Fields []GqlObjField
}

type GqlObjField struct {
	Name  string
	Value *GqlValue
}

type GqlArg struct {
	Name  string
	Value *GqlValue
}

type GqlDir struct {
	Name string
	Args []GqlArg
}

type GqlField struct {
	Name  string
	Alias string
	Args  []GqlArg
	Sels  []GqlSel
	Dirs  []GqlDir
}

type GqlSel struct {
	Kind   string // "field", "spread", "inline"
	Field  *GqlField
	Spread string
	Inline *string // nil = anonymous inline fragment, or type name string
	Sels   []GqlSel
	Dirs   []GqlDir
}

type GqlVarDef struct {
	Name string
	Def  *GqlValue
}

type GqlOp struct {
	Op      string // "query" or "mutation"
	Name    string
	VarDefs []GqlVarDef
	Sels    []GqlSel
}

type GqlDoc struct {
	Ops   []GqlOp
	Frags map[string]GqlFrag
}

type GqlFrag struct {
	Type string
	Sels []GqlSel
}

func gqlParse(src string) (GqlDoc, error) {
	i := 0
	err := func(m string) error {
		return fmt.Errorf("GraphQL parse error: %s at %d", m, i)
	}
	var skip func()
	skip = func() {
		for {
			for i < len(src) {
				c := src[i]
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' {
					i++
					continue
				}
				break
			}
			if i < len(src) && src[i] == '#' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			break
		}
	}
	peek := func() byte {
		skip()
		if i < len(src) {
			return src[i]
		}
		return 0
	}
	expect := func(c byte) error {
		skip()
		if i >= len(src) || src[i] != c {
			return err(fmt.Sprintf("expected %c", c))
		}
		i++
		return nil
	}
	parseName := func() (string, error) {
		skip()
		start := i
		if i < len(src) {
			c := src[i]
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
				i++
				for i < len(src) {
					cc := src[i]
					if (cc >= 'A' && cc <= 'Z') || (cc >= 'a' && cc <= 'z') || (cc >= '0' && cc <= '9') || cc == '_' {
						i++
					} else {
						break
					}
				}
				return src[start:i], nil
			}
		}
		return "", err("expected name")
	}

	parseStr := func() (string, error) {
		skip()
		if i+3 <= len(src) && src[i:i+3] == `"""` {
			end := strings.Index(src[i+3:], `"""`)
			if end < 0 {
				return "", err("unterminated block string")
			}
			v := src[i+3 : i+3+end]
			i = i + 3 + end + 3
			return v, nil
		}
		if i >= len(src) || src[i] != '"' {
			return "", err("expected string")
		}
		j := i + 1
		var out strings.Builder
		for j < len(src) && src[j] != '"' {
			if src[j] == '\\' {
				if j+1 >= len(src) {
					return "", err("unterminated escape sequence")
				}
				e := src[j+1]
				if e == 'n' {
					out.WriteByte('\n')
				} else if e == 't' {
					out.WriteByte('\t')
				} else if e == 'r' {
					out.WriteByte('\r')
				} else if e == 'u' {
					if j+6 > len(src) {
						return "", err("invalid unicode escape")
					}
					hex := src[j+2 : j+6]
					var r rune
					fmt.Sscanf(hex, "%x", &r)
					out.WriteRune(r)
					j += 6
					continue
				} else {
					out.WriteByte(e)
				}
				j += 2
			} else {
				out.WriteByte(src[j])
				j++
			}
		}
		if j >= len(src) {
			return "", err("unterminated string")
		}
		i = j + 1
		return out.String(), nil
	}

	var parseValue func() (*GqlValue, error)
	var parseArgs func() ([]GqlArg, error)
	var parseDirs func() ([]GqlDir, error)

	parseValue = func() (*GqlValue, error) {
		skip()
		if i >= len(src) {
			return nil, err("expected value")
		}
		c := src[i]
		if c == '$' {
			i++
			n, errName := parseName()
			if errName != nil {
				return nil, errName
			}
			return &GqlValue{K: "var", Name: n}, nil
		}
		if c == '"' {
			s, errStr := parseStr()
			if errStr != nil {
				return nil, errStr
			}
			return &GqlValue{K: "str", V: s}, nil
		}
		if c == '[' {
			i++
			var items []*GqlValue
			for peek() != ']' {
				v, errVal := parseValue()
				if errVal != nil {
					return nil, errVal
				}
				items = append(items, v)
			}
			if errExp := expect(']'); errExp != nil {
				return nil, errExp
			}
			return &GqlValue{K: "list", Items: items}, nil
		}
		if c == '{' {
			i++
			var fields []GqlObjField
			for peek() != '}' {
				n, errName := parseName()
				if errName != nil {
					return nil, errName
				}
				if errCol := expect(':'); errCol != nil {
					return nil, errCol
				}
				val, errVal := parseValue()
				if errVal != nil {
					return nil, errVal
				}
				fields = append(fields, GqlObjField{Name: n, Value: val})
			}
			if errExp := expect('}'); errExp != nil {
				return nil, errExp
			}
			return &GqlValue{K: "obj", Fields: fields}, nil
		}
		if c == '-' || (c >= '0' && c <= '9') {
			start := i
			if c == '-' {
				i++
			}
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			if i < len(src) && src[i] == '.' {
				i++
				for i < len(src) && src[i] >= '0' && src[i] <= '9' {
					i++
				}
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				i++
				if i < len(src) && (src[i] == '+' || src[i] == '-') {
					i++
				}
				for i < len(src) && src[i] >= '0' && src[i] <= '9' {
					i++
				}
			}
			numStr := src[start:i]
			var f float64
			fmt.Sscanf(numStr, "%f", &f)
			return &GqlValue{K: "num", V: f}, nil
		}
		n, errName := parseName()
		if errName != nil {
			return nil, errName
		}
		if n == "true" {
			return &GqlValue{K: "bool", V: true}, nil
		}
		if n == "false" {
			return &GqlValue{K: "bool", V: false}, nil
		}
		if n == "null" {
			return &GqlValue{K: "null"}, nil
		}
		return &GqlValue{K: "str", V: n}, nil
	}

	var parseTypeRefStr func() (string, error)
	parseTypeRefStr = func() (string, error) {
		skip()
		var t string
		if peek() == '[' {
			i++
			inner, errT := parseTypeRefStr()
			if errT != nil {
				return "", errT
			}
			if errExp := expect(']'); errExp != nil {
				return "", errExp
			}
			t = "[" + inner + "]"
		} else {
			n, errName := parseName()
			if errName != nil {
				return "", errName
			}
			t = n
		}
		skip()
		if i < len(src) && src[i] == '!' {
			i++
			t += "!"
		}
		return t, nil
	}

	parseArgs = func() ([]GqlArg, error) {
		var out []GqlArg
		if peek() != '(' {
			return out, nil
		}
		i++
		for peek() != ')' {
			n, errName := parseName()
			if errName != nil {
				return nil, errName
			}
			if errCol := expect(':'); errCol != nil {
				return nil, errCol
			}
			val, errVal := parseValue()
			if errVal != nil {
				return nil, errVal
			}
			out = append(out, GqlArg{Name: n, Value: val})
		}
		if errExp := expect(')'); errExp != nil {
			return nil, errExp
		}
		return out, nil
	}

	parseDirs = func() ([]GqlDir, error) {
		var out []GqlDir
		for peek() == '@' {
			i++
			n, errName := parseName()
			if errName != nil {
				return nil, errName
			}
			a, errArgs := parseArgs()
			if errArgs != nil {
				return nil, errArgs
			}
			out = append(out, GqlDir{Name: n, Args: a})
		}
		return out, nil
	}

	var parseSelSetFn func() ([]GqlSel, error)
	parseSelSetFn = func() ([]GqlSel, error) {
		if errExp := expect('{'); errExp != nil {
			return nil, errExp
		}
		var out []GqlSel
		for peek() != '}' {
			skip()
			if i+3 <= len(src) && src[i:i+3] == "..." {
				i += 3
				skip()
				if i+2 <= len(src) && src[i:i+2] == "on" && !((src[i+2] >= 'A' && src[i+2] <= 'Z') || (src[i+2] >= 'a' && src[i+2] <= 'z') || (src[i+2] >= '0' && src[i+2] <= '9') || src[i+2] == '_') {
					i += 2
					tName, errTN := parseName()
					if errTN != nil {
						return nil, errTN
					}
					d, errDirs := parseDirs()
					if errDirs != nil {
						return nil, errDirs
					}
					sels, errSels := parseSelSetFn()
					if errSels != nil {
						return nil, errSels
					}
					out = append(out, GqlSel{Kind: "inline", Inline: &tName, Sels: sels, Dirs: d})
				} else if peek() == '{' {
					d, errDirs := parseDirs()
					if errDirs != nil {
						return nil, errDirs
					}
					sels, errSels := parseSelSetFn()
					if errSels != nil {
						return nil, errSels
					}
					out = append(out, GqlSel{Kind: "inline", Inline: nil, Sels: sels, Dirs: d})
				} else {
					n, errName := parseName()
					if errName != nil {
						return nil, errName
					}
					d, errDirs := parseDirs()
					if errDirs != nil {
						return nil, errDirs
					}
					out = append(out, GqlSel{Kind: "spread", Spread: n, Dirs: d})
				}
				continue
			}

			n, errName := parseName()
			if errName != nil {
				return nil, errName
			}
			var alias string
			if peek() == ':' {
				i++
				alias = n
				n, errName = parseName()
				if errName != nil {
					return nil, errName
				}
			}
			a, errArgs := parseArgs()
			if errArgs != nil {
				return nil, errArgs
			}
			d, errDirs := parseDirs()
			if errDirs != nil {
				return nil, errDirs
			}
			var sels []GqlSel
			if peek() == '{' {
				var errSels error
				sels, errSels = parseSelSetFn()
				if errSels != nil {
					return nil, errSels
				}
			}
			field := &GqlField{Name: n, Alias: alias, Args: a, Sels: sels, Dirs: d}
			out = append(out, GqlSel{Kind: "field", Field: field})
		}
		if errExp := expect('}'); errExp != nil {
			return nil, errExp
		}
		return out, nil
	}

	doc := GqlDoc{Frags: make(map[string]GqlFrag)}
	for {
		skip()
		if i >= len(src) {
			break
		}
		if peek() == '{' {
			sels, errSels := parseSelSetFn()
			if errSels != nil {
				return doc, errSels
			}
			doc.Ops = append(doc.Ops, GqlOp{Op: "query", Sels: sels})
			continue
		}
		kw, errKW := parseName()
		if errKW != nil {
			return doc, errKW
		}
		if kw == "query" || kw == "mutation" || kw == "subscription" {
			var opName string
			if peek() >= 'A' && peek() <= 'Z' || peek() >= 'a' && peek() <= 'z' || peek() == '_' {
				opName, _ = parseName()
			}
			var varDefs []GqlVarDef
			if peek() == '(' {
				i++
				for peek() != ')' {
					if errExp := expect('$'); errExp != nil {
						return doc, errExp
					}
					vn, errVN := parseName()
					if errVN != nil {
						return doc, errVN
					}
					if errCol := expect(':'); errCol != nil {
						return doc, errCol
					}
					if _, errT := parseTypeRefStr(); errT != nil {
						return doc, errT
					}
					var def *GqlValue
					skip()
					if i < len(src) && src[i] == '=' {
						i++
						var errVal error
						def, errVal = parseValue()
						if errVal != nil {
							return doc, errVal
						}
					}
					varDefs = append(varDefs, GqlVarDef{Name: vn, Def: def})
					parseDirs()
				}
				if errExp := expect(')'); errExp != nil {
					return doc, errExp
				}
			}
			parseDirs()
			sels, errSels := parseSelSetFn()
			if errSels != nil {
				return doc, errSels
			}
			opType := "query"
			if kw == "mutation" {
				opType = "mutation"
			}
			doc.Ops = append(doc.Ops, GqlOp{Op: opType, Name: opName, VarDefs: varDefs, Sels: sels})
		} else if kw == "fragment" {
			fn, errFN := parseName()
			if errFN != nil {
				return doc, errFN
			}
			skip()
			on, errON := parseName()
			if errON != nil || on != "on" {
				return doc, err("expected on")
			}
			t, errT := parseName()
			if errT != nil {
				return doc, errT
			}
			parseDirs()
			sels, errSels := parseSelSetFn()
			if errSels != nil {
				return doc, errSels
			}
			doc.Frags[fn] = GqlFrag{Type: t, Sels: sels}
		} else {
			return doc, err(fmt.Sprintf("unexpected %s", kw))
		}
	}
	return doc, nil
}

func gqlEval(v *GqlValue, vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.K {
	case "var":
		return vars[v.Name]
	case "str", "num", "bool":
		return v.V
	case "null":
		return nil
	case "list":
		out := make([]any, len(v.Items))
		for idx, item := range v.Items {
			out[idx] = gqlEval(item, vars)
		}
		return out
	case "obj":
		out := make(map[string]any)
		for _, f := range v.Fields {
			out[f.Name] = gqlEval(f.Value, vars)
		}
		return out
	}
	return nil
}

type TypeRef struct {
	Kind string // "NAMED", "LIST", "NON_NULL"
	Name string
	Of   *TypeRef
}

func parseTypeRef(s string) TypeRef {
	t := strings.TrimSpace(s)
	if strings.HasSuffix(t, "!") {
		inner := parseTypeRef(t[:len(t)-1])
		return TypeRef{Kind: "NON_NULL", Of: &inner}
	}
	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
		inner := parseTypeRef(t[1 : len(t)-1])
		return TypeRef{Kind: "LIST", Of: &inner}
	}
	return TypeRef{Kind: "NAMED", Name: t}
}

func namedOf(ref TypeRef) string {
	if ref.Kind == "NAMED" {
		return ref.Name
	}
	if ref.Of != nil {
		return namedOf(*ref.Of)
	}
	return ""
}

type GqlFieldDef struct {
	Type    string
	Args    map[string]string
	Resolve func(src any, args map[string]any, ctx *GqlCtx) any
}

type GqlTypeDef struct {
	Kind          string // 'OBJECT' | 'INTERFACE' | 'UNION' | 'ENUM' | 'INPUT_OBJECT' | 'SCALAR'
	Fields        map[string]GqlFieldDef
	PossibleTypes []string
	EnumValues    []string
	InputFields   map[string]string
	ResolveType   func(src any) string
}

type GqlCtx struct {
	Vars   map[string]any
	Errors []GqlError
	Doc    GqlDoc
}

type GqlError struct {
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

type GqlSchema map[string]GqlTypeDef

var scalarsSet = map[string]bool{
	"String": true, "Int": true, "Float": true, "Boolean": true, "ID": true,
	"DateTime": true, "URI": true, "GitObjectID": true, "Base64String": true,
	"HTML": true, "GitTimestamp": true, "PreciseDateTime": true,
	"X509Certificate": true, "GitSSHRemote": true, "GitRefname": true,
}

func gqlDirOk(dirs []GqlDir, vars map[string]any) bool {
	for _, d := range dirs {
		var ifVal any
		for _, a := range d.Args {
			if a.Name == "if" {
				ifVal = gqlEval(a.Value, vars)
				break
			}
		}
		if d.Name == "skip" && ifVal == true {
			return false
		}
		if d.Name == "include" && ifVal == false {
			return false
		}
	}
	return true
}

func gqlExecSet(schema GqlSchema, typeName string, src any, sels []GqlSel, ctx *GqlCtx, path []any) map[string]any {
	out := make(map[string]any)
	runtimeType := typeName
	if srcMap, ok := src.(map[string]any); ok {
		if gt, ok := srcMap["__gqlType"].(string); ok && gt != "" {
			runtimeType = gt
		}
	}
	typeDef := schema[typeName]

	runField := func(f *GqlField) {
		key := f.Name
		if f.Alias != "" {
			key = f.Alias
		}
		fpath := make([]any, len(path)+1)
		copy(fpath, path)
		fpath[len(path)] = key

		if !gqlDirOk(f.Dirs, ctx.Vars) {
			return
		}
		if f.Name == "__typename" {
			out[key] = runtimeType
			return
		}
		if f.Name == "__schema" && typeName == "Query" {
			out[key] = complete(schema, parseTypeRef("__Schema"), map[string]any{"__gqlType": "__Schema", "schema": schema}, f, ctx, fpath)
			return
		}
		if f.Name == "__type" && typeName == "Query" {
			args := evalArgs(f, ctx)
			var typeNameArg string
			if tn, ok := args["name"].(string); ok {
				typeNameArg = tn
			}
			var t any
			if _, exists := schema[typeNameArg]; exists {
				t = map[string]any{"__gqlType": "__Type", "typeName": typeNameArg}
			}
			out[key] = complete(schema, parseTypeRef("__Type"), t, f, ctx, fpath)
			return
		}

		def, ok := typeDef.Fields[f.Name]
		if !ok && runtimeType != typeName {
			if rtDef, ok2 := schema[runtimeType]; ok2 {
				def, ok = rtDef.Fields[f.Name]
			}
		}
		if !ok {
			ctx.Errors = append(ctx.Errors, GqlError{
				Message: fmt.Sprintf("Field '%s' doesn't exist on type '%s'", f.Name, typeName),
				Path:    fpath,
			})
			out[key] = nil
			return
		}

		var val any
		func() {
			defer func() {
				if r := recover(); r != nil {
					ctx.Errors = append(ctx.Errors, GqlError{
						Message: fmt.Sprintf("%v", r),
						Path:    fpath,
					})
					val = nil
				}
			}()
			val = def.Resolve(src, evalArgs(f, ctx), ctx)
		}()
		out[key] = complete(schema, parseTypeRef(def.Type), val, f, ctx, fpath)
	}

	var walk func(list []GqlSel, currentType string)
	walk = func(list []GqlSel, currentType string) {
		for _, sel := range list {
			if sel.Kind == "spread" {
				frag, ok := ctx.Doc.Frags[sel.Spread]
				if ok {
					fragDef := schema[frag.Type]
					if frag.Type == currentType || frag.Type == runtimeType || fragDef.Kind != "OBJECT" {
						walk(frag.Sels, frag.Type)
					}
				}
				continue
			}
			if sel.Kind == "inline" {
				inlineType := currentType
				if sel.Inline != nil {
					inlineType = *sel.Inline
				}
				if sel.Inline == nil || inlineType == currentType || inlineType == runtimeType {
					walk(sel.Sels, inlineType)
				}
				continue
			}
			if sel.Field != nil {
				runField(sel.Field)
			}
		}
	}
	walk(sels, typeName)
	return out
}

func evalArgs(f *GqlField, ctx *GqlCtx) map[string]any {
	out := make(map[string]any)
	for _, a := range f.Args {
		out[a.Name] = gqlEval(a.Value, ctx.Vars)
	}
	return out
}

func complete(schema GqlSchema, ref TypeRef, val any, f *GqlField, ctx *GqlCtx, path []any) any {
	if ref.Kind == "NON_NULL" {
		inner := complete(schema, *ref.Of, val, f, ctx, path)
		if inner == nil {
			ctx.Errors = append(ctx.Errors, GqlError{
				Message: "Cannot return null for non-nullable field",
				Path:    path,
			})
		}
		return inner
	}
	if val == nil {
		return nil
	}
	if ref.Kind == "LIST" {
		arr, ok := val.([]any)
		if !ok {
			if reflectSlice := reflectValIsSlice(val); reflectSlice != nil {
				arr = reflectSlice
			} else {
				arr = []any{val}
			}
		}
		out := make([]any, len(arr))
		for idx, item := range arr {
			ipath := make([]any, len(path)+1)
			copy(ipath, path)
			ipath[len(path)] = idx
			out[idx] = complete(schema, *ref.Of, item, f, ctx, ipath)
		}
		return out
	}

	name := ref.Name
	if scalarsSet[name] || !schemaHas(schema, name) {
		if name == "ID" {
			return fmt.Sprintf("%v", val)
		}
		if name == "Int" {
			switch n := val.(type) {
			case int:
				return n
			case float64:
				return int64(n)
			case int64:
				return n
			case float32:
				return int64(n)
			case string:
				var fi float64
				if _, err := fmt.Sscanf(n, "%f", &fi); err == nil {
					return int64(fi)
				}
			}
			return val
		}
		if name == "Boolean" {
			switch b := val.(type) {
			case bool:
				return b
			case int, int64, float64:
				return b != 0
			case string:
				return b == "true" || b == "1"
			}
			return val
		}
		return val
	}

	def := schema[name]
	if def.Kind == "ENUM" {
		return val
	}
	if f == nil || len(f.Sels) == 0 {
		return val
	}

	runtime := name
	src := val
	if def.Kind == "INTERFACE" || def.Kind == "UNION" {
		if def.ResolveType != nil {
			runtime = def.ResolveType(src)
		} else if srcMap, ok := src.(map[string]any); ok {
			if gt, ok := srcMap["__gqlType"].(string); ok && gt != "" {
				runtime = gt
			}
		}
	} else if srcMap, ok := src.(map[string]any); ok {
		if gt, ok := srcMap["__gqlType"].(string); ok && gt != "" {
			runtime = gt
		}
	}
	return gqlExecSet(schema, runtime, src, f.Sels, ctx, path)
}

func reflectValIsSlice(v any) []any {
	if v == nil {
		return nil
	}
	switch s := v.(type) {
	case []any:
		return s
	case []string:
		out := make([]any, len(s))
		for i, x := range s {
			out[i] = x
		}
		return out
	case []int:
		out := make([]any, len(s))
		for i, x := range s {
			out[i] = x
		}
		return out
	case []map[string]any:
		out := make([]any, len(s))
		for i, x := range s {
			out[i] = x
		}
		return out
	}
	return nil
}

func schemaHas(schema GqlSchema, name string) bool {
	_, ok := schema[name]
	return ok
}

func gqlRun(schema GqlSchema, query string, variables map[string]any, operationName string) map[string]any {
	doc, err := gqlParse(query)
	if err != nil {
		return map[string]any{
			"errors": []GqlError{{Message: err.Error()}},
		}
	}
	var op *GqlOp
	if operationName != "" {
		for _, o := range doc.Ops {
			if o.Name == operationName {
				op = &o
				break
			}
		}
	} else if len(doc.Ops) > 0 {
		op = &doc.Ops[0]
	}
	if op == nil {
		return map[string]any{
			"errors": []GqlError{{Message: "no operation found"}},
		}
	}

	vars := make(map[string]any)
	for k, v := range variables {
		vars[k] = v
	}
	for _, vd := range op.VarDefs {
		if vars[vd.Name] == nil && vd.Def != nil {
			vars[vd.Name] = gqlEval(vd.Def, vars)
		}
	}

	ctx := &GqlCtx{
		Vars:   vars,
		Errors: []GqlError{},
		Doc:    doc,
	}
	rootType := "Query"
	if op.Op == "mutation" {
		rootType = "Mutation"
	}
	data := gqlExecSet(schema, rootType, map[string]any{"__gqlType": rootType}, op.Sels, ctx, nil)
	out := map[string]any{"data": data}
	if len(ctx.Errors) > 0 {
		out["errors"] = ctx.Errors
	}
	return out
}

func handleGraphQL(w http.ResponseWriter, raw []byte) {
	Mu.Lock()
	defer Mu.Unlock()

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		sendJson(w, 400, map[string]any{
			"errors": []GqlError{{Message: "Problems parsing JSON"}},
		}, nil)
		return
	}

	runOne := func(item any) any {
		obj, ok := item.(map[string]any)
		if !ok || obj == nil {
			return map[string]any{"errors": []GqlError{{Message: "invalid request"}}}
		}
		query, _ := obj["query"].(string)
		if query == "" {
			return map[string]any{"errors": []GqlError{{Message: "query is required"}}}
		}
		vars := make(map[string]any)
		if vMap, ok := obj["variables"].(map[string]any); ok {
			vars = vMap
		}
		var opName string
		if on, ok := obj["operationName"].(string); ok {
			opName = on
		}
		return gqlRun(FORGE_SCHEMA, query, vars, opName)
	}

	if arr, ok := parsed.([]any); ok {
		results := make([]any, len(arr))
		for i, item := range arr {
			results[i] = runOne(item)
		}
		sendJson(w, 200, results, nil)
		return
	}

	sendJson(w, 200, runOne(parsed), nil)
}

func buildIntrospection(schema GqlSchema) {
	typeWrapper := func(name string) any {
		return map[string]any{"__gqlType": "__Type", "typeName": name}
	}
	refToWrapper := func(ref TypeRef) any {
		if ref.Kind == "NAMED" {
			return typeWrapper(ref.Name)
		}
		return map[string]any{"__gqlType": "__Type", "wrapper": ref}
	}

	schema["__Type"] = GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"name": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m, ok := s.(map[string]any)
					if !ok {
						return nil
					}
					if _, hasWrap := m["wrapper"]; hasWrap {
						return nil
					}
					return m["typeName"]
				},
			},
			"kind": {
				Type: "__TypeKind!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m, ok := s.(map[string]any)
					if !ok {
						return "OBJECT"
					}
					if wrap, hasWrap := m["wrapper"].(TypeRef); hasWrap {
						return wrap.Kind
					}
					tn, _ := m["typeName"].(string)
					if scalarsSet[tn] {
						return "SCALAR"
					}
					if def, ok := schema[tn]; ok {
						return def.Kind
					}
					return "OBJECT"
				},
			},
			"description": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
			"fields": {
				Type: "[__Field]",
				Args: map[string]string{"includeDeprecated": "Boolean"},
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m, ok := s.(map[string]any)
					if !ok {
						return nil
					}
					if _, hasWrap := m["wrapper"]; hasWrap {
						return nil
					}
					tn, _ := m["typeName"].(string)
					def, ok := schema[tn]
					if !ok || def.Fields == nil {
						return nil
					}
					out := make([]any, 0, len(def.Fields))
					for fname, fdef := range def.Fields {
						out = append(out, map[string]any{
							"__gqlType": "__Field",
							"name":      fname,
							"def":       fdef,
						})
					}
					return out
				},
			},
			"inputFields": {
				Type: "[__InputValue]",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m, ok := s.(map[string]any)
					if !ok {
						return nil
					}
					if _, hasWrap := m["wrapper"]; hasWrap {
						return nil
					}
					tn, _ := m["typeName"].(string)
					def, ok := schema[tn]
					if !ok || def.InputFields == nil {
						return nil
					}
					out := make([]any, 0, len(def.InputFields))
					for n, t := range def.InputFields {
						out = append(out, map[string]any{
							"__gqlType": "__InputValue",
							"name":      n,
							"typeStr":   t,
						})
					}
					return out
				},
			},
			"enumValues": {
				Type: "[__EnumValue]",
				Args: map[string]string{"includeDeprecated": "Boolean"},
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m, ok := s.(map[string]any)
					if !ok {
						return nil
					}
					if _, hasWrap := m["wrapper"]; hasWrap {
						return nil
					}
					tn, _ := m["typeName"].(string)
					def, ok := schema[tn]
					if !ok || def.EnumValues == nil {
						return nil
					}
					out := make([]any, len(def.EnumValues))
					for i, n := range def.EnumValues {
						out[i] = map[string]any{
							"__gqlType": "__EnumValue",
							"name":      n,
						}
					}
					return out
				},
			},
			"possibleTypes": {
				Type: "[__Type]",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m, ok := s.(map[string]any)
					if !ok {
						return nil
					}
					if _, hasWrap := m["wrapper"]; hasWrap {
						return nil
					}
					tn, _ := m["typeName"].(string)
					def, ok := schema[tn]
					if !ok || def.PossibleTypes == nil {
						return nil
					}
					out := make([]any, len(def.PossibleTypes))
					for i, pt := range def.PossibleTypes {
						out[i] = typeWrapper(pt)
					}
					return out
				},
			},
			"ofType": {
				Type: "__Type",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m, ok := s.(map[string]any)
					if !ok {
						return nil
					}
					if wrap, hasWrap := m["wrapper"].(TypeRef); hasWrap && wrap.Of != nil {
						return refToWrapper(*wrap.Of)
					}
					return nil
				},
			},
			"interfaces": {
				Type: "[__Type]",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return []any{}
				},
			},
			"specifiedByURL": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
		},
	}

	schema["__Field"] = GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"name": {
				Type: "String!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return s.(map[string]any)["name"]
				},
			},
			"description": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
			"args": {
				Type: "[__InputValue!]!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m := s.(map[string]any)
					def := m["def"].(GqlFieldDef)
					if def.Args == nil {
						return []any{}
					}
					out := make([]any, 0, len(def.Args))
					for n, t := range def.Args {
						out = append(out, map[string]any{
							"__gqlType": "__InputValue",
							"name":      n,
							"typeStr":   t,
						})
					}
					return out
				},
			},
			"type": {
				Type: "__Type!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m := s.(map[string]any)
					def := m["def"].(GqlFieldDef)
					return refToWrapper(parseTypeRef(def.Type))
				},
			},
			"isDeprecated": {
				Type: "Boolean!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return false
				},
			},
			"deprecationReason": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
		},
	}

	schema["__InputValue"] = GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"name": {
				Type: "String!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return s.(map[string]any)["name"]
				},
			},
			"description": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
			"type": {
				Type: "__Type!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m := s.(map[string]any)
					tStr := m["typeStr"].(string)
					return refToWrapper(parseTypeRef(tStr))
				},
			},
			"defaultValue": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
			"isDeprecated": {
				Type: "Boolean!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return false
				},
			},
			"deprecationReason": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
		},
	}

	schema["__EnumValue"] = GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"name": {
				Type: "String!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return s.(map[string]any)["name"]
				},
			},
			"description": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
			"isDeprecated": {
				Type: "Boolean!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return false
				},
			},
			"deprecationReason": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
		},
	}

	schema["__Schema"] = GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"types": {
				Type: "[__Type!]!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					m := s.(map[string]any)
					sch := m["schema"].(GqlSchema)
					keys := make([]string, 0, len(sch))
					for k := range sch {
						if !strings.HasPrefix(k, "__") || k == "__Type" {
							keys = append(keys, k)
						}
					}
					sort.Strings(keys)
					out := make([]any, len(keys))
					for i, k := range keys {
						out[i] = typeWrapper(k)
					}
					return out
				},
			},
			"queryType": {
				Type: "__Type!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return typeWrapper("Query")
				},
			},
			"mutationType": {
				Type: "__Type",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return typeWrapper("Mutation")
				},
			},
			"subscriptionType": {
				Type: "__Type",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
			"directives": {
				Type: "[__Directive!]!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return []any{}
				},
			},
			"description": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
		},
	}

	schema["__TypeKind"] = GqlTypeDef{
		Kind:       "ENUM",
		EnumValues: []string{"SCALAR", "OBJECT", "INTERFACE", "UNION", "ENUM", "INPUT_OBJECT", "LIST", "NON_NULL"},
	}
}

func b64e(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func b64d(s string) string {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(data)
}

func repoNodeId(repo *Repo) string {
	return b64e(fmt.Sprintf("%d:Repository", repo.ID))
}

func issueNodeId(repo *Repo, rec *IssueRec) string {
	return b64e(fmt.Sprintf("%d:%d:Issue", repo.ID, rec.Number))
}

func prNodeId(repo *Repo, rec *IssueRec) string {
	return b64e(fmt.Sprintf("%d:%d:PullRequest", repo.ID, rec.Number))
}

func commentNodeId(repo *Repo, rec *IssueRec, c Comment) string {
	return b64e(fmt.Sprintf("%d:%d:%d:IssueComment", repo.ID, rec.Number, c.ID))
}

func userNodeId() string {
	return b64e(fmt.Sprintf("%d:User", ViewerID))
}

func repoHtmlUrl(repo *Repo) string {
	return fmt.Sprintf("http://forge/%s/%s", ViewerLogin, repo.Name)
}

func tsOf(rec *IssueRec) string {
	return isoOf(EpochSec + int64(rec.CreatedSeq))
}

func wrapRepo(repo *Repo) map[string]any {
	return map[string]any{"__gqlType": "Repository", "repo": repo}
}

func wrapIssue(repo *Repo, rec *IssueRec) map[string]any {
	return map[string]any{"__gqlType": "Issue", "repo": repo, "rec": rec}
}

func wrapPr(repo *Repo, rec *IssueRec) map[string]any {
	return map[string]any{"__gqlType": "PullRequest", "repo": repo, "rec": rec}
}

func wrapPull(repo *Repo, rec *IssueRec) map[string]any {
	return wrapPr(repo, rec)
}

func wrapComment(repo *Repo, rec *IssueRec, c Comment) map[string]any {
	return map[string]any{"__gqlType": "IssueComment", "repo": repo, "rec": rec, "comment": c}
}

func wrapUser() map[string]any {
	return map[string]any{"__gqlType": "User"}
}

func wrapGitObject(repo *Repo, sha string) map[string]any {
	obj, ok := repo.Objects[sha]
	t := "Blob"
	if ok {
		if obj.Type == ObjCommit {
			t = "Commit"
		} else if obj.Type == ObjTree {
			t = "Tree"
		}
	}
	return map[string]any{"__gqlType": t, "repo": repo, "sha": sha}
}

func recOf(s any) *IssueRec {
	if m, ok := s.(map[string]any); ok {
		if r, ok := m["rec"].(*IssueRec); ok {
			return r
		}
	}
	return nil
}

func commentOf(s any) Comment {
	if m, ok := s.(map[string]any); ok {
		if c, ok := m["comment"].(Comment); ok {
			return c
		}
	}
	return Comment{}
}

func shaOf(s any) string {
	if m, ok := s.(map[string]any); ok {
		if v, ok := m["sha"].(string); ok {
			return v
		}
	}
	return ""
}

func nameOf(s any) string {
	if m, ok := s.(map[string]any); ok {
		if v, ok := m["name"].(string); ok {
			return v
		}
	}
	return ""
}

func wrapLabel(repo *Repo, name string) map[string]any {
	return map[string]any{"__gqlType": "Label", "repo": repo, "name": name}
}

func wrapGitActor(name, email, date string) map[string]any {
	return map[string]any{"__gqlType": "GitActor", "name": name, "email": email, "date": date}
}

func commitInfoOf(repo *Repo, sha string) (CommitInfo, bool) {
	obj, ok := repo.Objects[sha]
	if !ok {
		return CommitInfo{}, false
	}
	return parseCommit(obj.Data), true
}

func abbrevSha(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func gqlPaginate(items []any, args map[string]any) map[string]any {
	cursorOf := func(idx int) string {
		return b64e(fmt.Sprintf("cursor:%d", idx))
	}
	idxOf := func(c any) int {
		cs, ok := c.(string)
		if !ok || cs == "" {
			return -1
		}
		decoded := b64d(cs)
		var num int
		if _, err := fmt.Sscanf(decoded, "cursor:%d", &num); err == nil {
			return num
		}
		return -1
	}

	start := 0
	end := len(items)
	if afterIdx := idxOf(args["after"]); afterIdx >= 0 {
		start = afterIdx + 1
	}
	if beforeIdx := idxOf(args["before"]); beforeIdx >= 0 {
		if beforeIdx < end {
			end = beforeIdx
		}
	}
	if start > end {
		start = end
	}
	slice := items[start:end]
	slicedStart := start

	if first, ok := args["first"].(float64); ok {
		f := int(first)
		if f < 0 {
			f = 0
		}
		if f < len(slice) {
			slice = slice[:f]
		}
	} else if first, ok := args["first"].(int); ok {
		f := first
		if f < 0 {
			f = 0
		}
		if f < len(slice) {
			slice = slice[:f]
		}
	}

	if last, ok := args["last"].(float64); ok {
		l := int(last)
		if l < 0 {
			l = 0
		}
		if l < len(slice) {
			cut := len(slice) - l
			if cut < 0 {
				cut = 0
			}
			slicedStart += cut
			slice = slice[cut:]
		}
	} else if last, ok := args["last"].(int); ok {
		l := last
		if l < 0 {
			l = 0
		}
		if l < len(slice) {
			cut := len(slice) - l
			if cut < 0 {
				cut = 0
			}
			slicedStart += cut
			slice = slice[cut:]
		}
	}

	edges := make([]any, len(slice))
	for i, node := range slice {
		edges[i] = map[string]any{
			"node":   node,
			"cursor": cursorOf(slicedStart + i),
		}
	}

	var startCursor any = nil
	var endCursor any = nil
	if len(edges) > 0 {
		startCursor = edges[0].(map[string]any)["cursor"]
		endCursor = edges[len(edges)-1].(map[string]any)["cursor"]
	}

	hasPreviousPage := slicedStart > start
	hasNextPage := slicedStart+len(slice) < end

	return map[string]any{
		"total": len(items),
		"page": map[string]any{
			"edges": edges,
			"nodes": slice,
			"pageInfo": map[string]any{
				"__gqlType":       "PageInfo",
				"hasNextPage":     hasNextPage,
				"hasPreviousPage": hasPreviousPage,
				"startCursor":     startCursor,
				"endCursor":       endCursor,
			},
		},
	}
}

func connFields(nodeType string) map[string]GqlFieldDef {
	return map[string]GqlFieldDef{
		"edges": {Type: fmt.Sprintf("[%sEdge]", nodeType), Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
			return s.(map[string]any)["page"].(map[string]any)["edges"]
		}},
		"nodes": {Type: fmt.Sprintf("[%s]", nodeType), Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
			return s.(map[string]any)["page"].(map[string]any)["nodes"]
		}},
		"pageInfo": {Type: "PageInfo!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
			return s.(map[string]any)["page"].(map[string]any)["pageInfo"]
		}},
		"totalCount": {Type: "Int!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["total"] }},
	}
}

func edgeFields(nodeType string) map[string]GqlFieldDef {
	return map[string]GqlFieldDef{
		"node":   {Type: nodeType, Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["node"] }},
		"cursor": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["cursor"] }},
	}
}

var CONN_ARGS = map[string]string{"first": "Int", "after": "String", "last": "Int", "before": "String"}

func sortByOrder(items []*IssueRec, args map[string]any) []*IssueRec {
	ob, _ := args["orderBy"].(map[string]any)
	dir := 1
	if ob != nil {
		if d, ok := ob["direction"].(string); ok && d == "DESC" {
			dir = -1
		}
	}
	sorted := make([]*IssueRec, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		diff := sorted[i].CreatedSeq - sorted[j].CreatedSeq
		if dir == -1 {
			return diff > 0
		}
		return diff < 0
	})
	return sorted
}

func repoById(id int) *Repo {
	for _, r := range repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func decodeNodeId(id string) any {
	raw := b64d(id)
	var repoID, num, commentID int
	if n, err := fmt.Sscanf(raw, "%d:Repository", &repoID); err == nil && n == 1 {
		repo := repoById(repoID)
		if repo != nil {
			return wrapRepo(repo)
		}
		return nil
	}
	if n, err := fmt.Sscanf(raw, "%d:%d:Issue", &repoID, &num); err == nil && n == 2 {
		repo := repoById(repoID)
		if repo != nil {
			for _, rec := range repo.Issues {
				if rec.Kind == "issue" && rec.Number == num {
					return wrapIssue(repo, rec)
				}
			}
		}
		return nil
	}
	if n, err := fmt.Sscanf(raw, "%d:%d:PullRequest", &repoID, &num); err == nil && n == 2 {
		repo := repoById(repoID)
		if repo != nil {
			for _, rec := range repo.Issues {
				if rec.Kind == "pull" && rec.Number == num {
					return wrapPr(repo, rec)
				}
			}
		}
		return nil
	}
	if n, err := fmt.Sscanf(raw, "%d:%d:%d:IssueComment", &repoID, &num, &commentID); err == nil && n == 3 {
		repo := repoById(repoID)
		if repo != nil {
			for _, rec := range repo.Issues {
				if rec.Number == num {
					for _, c := range rec.Comments {
						if c.ID == commentID {
							return wrapComment(repo, rec, c)
						}
					}
				}
			}
		}
		return nil
	}
	if n, err := fmt.Sscanf(raw, "%d:User", &repoID); err == nil && n == 1 {
		if repoID == ViewerID {
			return wrapUser()
		}
		return nil
	}
	return nil
}

func mergePullRec(repo *Repo, rec *IssueRec) any {
	if rec.Merged {
		if rec.HasMergeSha {
			return rec.MergeCommitSha
		}
		return nil
	}
	headSha := ""
	if rec.HeadRef != "" {
		headSha = branchTip(repo, rec.HeadRef)
	}
	baseSha := ""
	if rec.BaseRef != "" {
		baseSha = branchTip(repo, rec.BaseRef)
	}
	if headSha == "" || baseSha == "" {
		return nil
	}
	mergedTree, ok := mergeTrees(repo, baseSha, headSha)
	if !ok {
		return nil
	}
	commitSha := makeCommit(repo, mergedTree, []string{baseSha}, fmt.Sprintf("%s (#%d)", rec.Title, rec.Number))
	repo.Refs["refs/heads/"+rec.BaseRef] = commitSha
	rec.Merged = true
	rec.State = "closed"
	rec.MergeCommitSha = commitSha
	rec.HasMergeSha = true
	rec.MergedTs = nextTs()
	return commitSha
}

var ZERO_SHA = "0000000000000000000000000000000000000000"

func buildForgeSchema() GqlSchema {
	schema := make(GqlSchema)
	S := func(name string, def GqlTypeDef) {
		schema[name] = def
	}
	for _, sc := range []string{"String", "Int", "Float", "Boolean", "ID", "DateTime", "URI", "GitObjectID", "Base64String", "HTML", "GitTimestamp", "PreciseDateTime"} {
		S(sc, GqlTypeDef{Kind: "SCALAR"})
	}

	S("PageInfo", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"hasNextPage":     {Type: "Boolean!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["hasNextPage"] }},
			"hasPreviousPage": {Type: "Boolean!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["hasPreviousPage"] }},
			"startCursor":     {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["startCursor"] }},
			"endCursor":       {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["endCursor"] }},
		},
	})

	S("User", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id":         {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return userNodeId() }},
			"login":      {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return ViewerLogin }},
			"name":       {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return ViewerName }},
			"email":      {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return ViewerEmail }},
			"avatarUrl":  {Type: "URI!", Args: map[string]string{"size": "Int"}, Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }},
			"url":        {Type: "URI!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }},
			"databaseId": {Type: "Int", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return float64(ViewerID) }},
			"createdAt":  {Type: "DateTime!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return isoOf(EpochSec) }},
			"updatedAt":  {Type: "DateTime!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return isoOf(EpochSec) }},
			"isViewer":   {Type: "Boolean!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return true }},
			"repositories": {
				Type: "RepositoryConnection!",
				Args: map[string]string{"first": "Int", "after": "String", "last": "Int", "before": "String", "orderBy": "RepositoryOrder"},
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					reposSlice := make([]any, 0, len(repos))
					for _, r := range repos {
						reposSlice = append(reposSlice, wrapRepo(r))
					}
					return gqlPaginate(reposSlice, args)
				},
			},
		},
	})

	S("RepositoryOwner", GqlTypeDef{Kind: "INTERFACE", PossibleTypes: []string{"User"}, ResolveType: func(s any) string { return "User" }, Fields: map[string]GqlFieldDef{"login": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return ViewerLogin }}, "id": {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return userNodeId() }}, "url": {Type: "URI!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }}}})
	S("Node", GqlTypeDef{Kind: "INTERFACE", PossibleTypes: []string{"Repository", "Issue", "PullRequest", "User", "IssueComment"}, ResolveType: func(s any) string { return s.(map[string]any)["__gqlType"].(string) }, Fields: map[string]GqlFieldDef{"id": {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }}}})
	S("Actor", GqlTypeDef{Kind: "INTERFACE", PossibleTypes: []string{"User"}, ResolveType: func(s any) string { return "User" }, Fields: map[string]GqlFieldDef{"login": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return ViewerLogin }}, "id": {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return userNodeId() }}, "url": {Type: "URI!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }}, "avatarUrl": {Type: "URI!", Args: map[string]string{"size": "Int"}, Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }}}})
	S("RepositoryConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("Repository")})
	S("RepositoryEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("Repository")})

	repoFrom := func(s any) *Repo {
		if m, ok := s.(map[string]any); ok {
			if r, ok := m["repo"].(*Repo); ok {
				return r
			}
		}
		if r, ok := s.(*Repo); ok {
			return r
		}
		return nil
	}

	mergeArgs := func(base map[string]string, extra map[string]string) map[string]string {
		m := make(map[string]string, len(base)+len(extra))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	sortByOrder := func(items []*IssueRec, args map[string]any) []*IssueRec {
		if len(items) <= 1 {
			return items
		}
		res := make([]*IssueRec, len(items))
		copy(res, items)
		ob, _ := args["orderBy"].(map[string]any)
		field := "CREATED_AT"
		dir := "ASC"
		if ob != nil {
			if f, ok := ob["field"].(string); ok && f != "" {
				field = strings.ToUpper(f)
			}
			if d, ok := ob["direction"].(string); ok && d != "" {
				dir = strings.ToUpper(d)
			}
		}
		sort.SliceStable(res, func(i, j int) bool {
			a, b := res[i], res[j]
			var va, vb int64
			switch field {
			case "UPDATED_AT":
				va = int64(a.CreatedSeq)
				if va == 0 {
					va = int64(a.Number)
				}
				vb = int64(b.CreatedSeq)
				if vb == 0 {
					vb = int64(b.Number)
				}
			case "COMMENTS":
				va = int64(len(a.Comments))
				vb = int64(len(b.Comments))
			default: // CREATED_AT
				va = int64(a.CreatedSeq)
				if va == 0 {
					va = int64(a.Number)
				}
				vb = int64(b.CreatedSeq)
				if vb == 0 {
					vb = int64(b.Number)
				}
			}
			if dir == "DESC" {
				return va > vb
			}
			return va < vb
		})
		return res
	}

	repoExtraFields := map[string]GqlFieldDef{
		"viewerPermission": {
			Type: "RepositoryPermission!",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return "ADMIN"
			},
		},
		"hasIssuesEnabled": {
			Type: "Boolean!",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return true
			},
		},
		"hasProjectsEnabled": {
			Type: "Boolean!",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return false
			},
		},
		"hasWikiEnabled": {
			Type: "Boolean!",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return false
			},
		},
		"stargazerCount": {
			Type: "Int!",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return 0
			},
		},
		"forkCount": {
			Type: "Int!",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return 0
			},
		},
		"diskUsage": {
			Type: "Int",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return 0
			},
		},
		"primaryLanguage": {
			Type: "Language",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return nil
			},
		},
		"licenseInfo": {
			Type: "License",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return nil
			},
		},
		"issues": {
			Type: "IssueConnection!",
			Args: mergeArgs(CONN_ARGS, map[string]string{
				"states":   "[IssueState!]",
				"orderBy":  "IssueOrder",
				"labels":   "[String!]",
				"filterBy": "IssueFilters",
			}),
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo == nil {
					return gqlPaginate(nil, args)
				}
				var items []*IssueRec
				for _, r := range repo.Issues {
					if r.Kind == "issue" {
						items = append(items, r)
					}
				}
				if rawStates, ok := args["states"].([]any); ok && len(rawStates) > 0 {
					want := make(map[string]bool)
					for _, x := range rawStates {
						want[strings.ToLower(fmt.Sprint(x))] = true
					}
					var filtered []*IssueRec
					for _, r := range items {
						if want[strings.ToLower(r.State)] {
							filtered = append(filtered, r)
						}
					}
					items = filtered
				}
				if fb, ok := args["filterBy"].(map[string]any); ok {
					if rawStates, ok := fb["states"].([]any); ok && len(rawStates) > 0 {
						want := make(map[string]bool)
						for _, x := range rawStates {
							want[strings.ToLower(fmt.Sprint(x))] = true
						}
						var filtered []*IssueRec
						for _, r := range items {
							if want[strings.ToLower(r.State)] {
								filtered = append(filtered, r)
							}
						}
						items = filtered
					}
				}
				if rawLabels, ok := args["labels"].([]any); ok && len(rawLabels) > 0 {
					var labels []string
					for _, l := range rawLabels {
						labels = append(labels, fmt.Sprint(l))
					}
					var filtered []*IssueRec
					for _, r := range items {
						hasAll := true
						for _, l := range labels {
							found := false
							for _, rl := range r.Labels {
								if rl == l {
									found = true
									break
								}
							}
							if !found {
								hasAll = false
								break
							}
						}
						if hasAll {
							filtered = append(filtered, r)
						}
					}
					items = filtered
				}
				sorted := sortByOrder(items, args)
				var nodes []any
				for _, r := range sorted {
					nodes = append(nodes, wrapIssue(repo, r))
				}
				return gqlPaginate(nodes, args)
			},
		},
		"issue": {
			Type: "Issue",
			Args: map[string]string{"number": "Int!"},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo == nil {
					return nil
				}
				var num int
				switch v := args["number"].(type) {
				case int:
					num = v
				case float64:
					num = int(v)
				case int64:
					num = int(v)
				}
				for _, r := range repo.Issues {
					if r.Kind == "issue" && r.Number == num {
						return wrapIssue(repo, r)
					}
				}
				return nil
			},
		},
		"issueOrPullRequest": {
			Type: "IssueOrPullRequest",
			Args: map[string]string{"number": "Int!"},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo == nil {
					return nil
				}
				var num int
				switch v := args["number"].(type) {
				case int:
					num = v
				case float64:
					num = int(v)
				case int64:
					num = int(v)
				}
				for _, r := range repo.Issues {
					if r.Number == num {
						if r.Kind == "pull" {
							return wrapPull(repo, r)
						}
						return wrapIssue(repo, r)
					}
				}
				return nil
			},
		},
		"pullRequests": {
			Type: "PullRequestConnection!",
			Args: mergeArgs(CONN_ARGS, map[string]string{
				"states":      "[PullRequestState!]",
				"orderBy":     "IssueOrder",
				"baseRefName": "String",
				"headRefName": "String",
				"labels":      "[String!]",
			}),
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo == nil {
					return gqlPaginate(nil, args)
				}
				var items []*IssueRec
				for _, r := range repo.Issues {
					if r.Kind == "pull" {
						items = append(items, r)
					}
				}
				if rawStates, ok := args["states"].([]any); ok && len(rawStates) > 0 {
					var filtered []*IssueRec
					for _, r := range items {
						match := false
						for _, x := range rawStates {
							sx := strings.ToUpper(fmt.Sprint(x))
							if sx == "MERGED" {
								if r.Merged {
									match = true
									break
								}
							} else {
								if !r.Merged && strings.ToLower(r.State) == strings.ToLower(sx) {
									match = true
									break
								}
							}
						}
						if match {
							filtered = append(filtered, r)
						}
					}
					items = filtered
				}
				if baseRef, ok := args["baseRefName"].(string); ok && baseRef != "" {
					var filtered []*IssueRec
					for _, r := range items {
						if r.BaseRef == baseRef {
							filtered = append(filtered, r)
						}
					}
					items = filtered
				}
				if headRef, ok := args["headRefName"].(string); ok && headRef != "" {
					var filtered []*IssueRec
					for _, r := range items {
						if r.HeadRef == headRef {
							filtered = append(filtered, r)
						}
					}
					items = filtered
				}
				if rawLabels, ok := args["labels"].([]any); ok && len(rawLabels) > 0 {
					var labels []string
					for _, l := range rawLabels {
						labels = append(labels, fmt.Sprint(l))
					}
					var filtered []*IssueRec
					for _, r := range items {
						hasAll := true
						for _, l := range labels {
							found := false
							for _, rl := range r.Labels {
								if rl == l {
									found = true
									break
								}
							}
							if !found {
								hasAll = false
								break
							}
						}
						if hasAll {
							filtered = append(filtered, r)
						}
					}
					items = filtered
				}
				sorted := sortByOrder(items, args)
				var nodes []any
				for _, r := range sorted {
					nodes = append(nodes, wrapPull(repo, r))
				}
				return gqlPaginate(nodes, args)
			},
		},
		"pullRequest": {
			Type: "PullRequest",
			Args: map[string]string{"number": "Int!"},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo == nil {
					return nil
				}
				var num int
				switch v := args["number"].(type) {
				case int:
					num = v
				case float64:
					num = int(v)
				case int64:
					num = int(v)
				}
				for _, r := range repo.Issues {
					if r.Kind == "pull" && r.Number == num {
						return wrapPull(repo, r)
					}
				}
				return nil
			},
		},
		"refs": {
			Type: "RefConnection",
			Args: mergeArgs(CONN_ARGS, map[string]string{"refPrefix": "String!"}),
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo == nil {
					return gqlPaginate(nil, args)
				}
				prefix := "refs/heads/"
				if p, ok := args["refPrefix"].(string); ok && p != "" {
					prefix = p
				}
				var names []string
				for k := range repo.Refs {
					if strings.HasPrefix(k, prefix) {
						names = append(names, k)
					}
				}
				sort.Strings(names)
				var nodes []any
				for _, k := range names {
					nodes = append(nodes, map[string]any{
						"__gqlType": "Ref",
						"repo":      repo,
						"name":      k,
					})
				}
				return gqlPaginate(nodes, args)
			},
		},
		"ref": {
			Type: "Ref",
			Args: map[string]string{"qualifiedName": "String!"},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo == nil {
					return nil
				}
				q := fmt.Sprint(args["qualifiedName"])
				full := q
				if !strings.HasPrefix(full, "refs/") {
					full = "refs/heads/" + full
				}
				if _, ok := repo.Refs[full]; ok {
					return map[string]any{
						"__gqlType": "Ref",
						"repo":      repo,
						"name":      full,
					}
				}
				return nil
			},
		},
		"object": {
			Type: "GitObject",
			Args: map[string]string{"expression": "String!"},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo == nil {
					return nil
				}
				expr := fmt.Sprint(args["expression"])
				colon := strings.Index(expr, ":")
				rev := expr
				path := ""
				if colon >= 0 {
					rev = expr[:colon]
					path = expr[colon+1:]
				}
				var sha string
				isHex40 := func(h string) bool {
					if len(h) != 40 {
						return false
					}
					for i := 0; i < 40; i++ {
						c := h[i]
						if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
							return false
						}
					}
					return true
				}
				if isHex40(rev) {
					sha = rev
				} else if rev == "HEAD" {
					sha = branchTip(repo, repo.DefaultBranch)
				} else {
					sha = branchTip(repo, rev)
					if sha == "" {
						refKey := rev
						if !strings.HasPrefix(refKey, "refs/") {
							refKey = "refs/heads/" + refKey
						}
						sha = repo.Refs[refKey]
					}
				}
				if sha == "" {
					return nil
				}
				obj, objOk := repo.Objects[sha]
				if objOk && obj.Type == ObjCommit {
					if path == "" {
						return wrapGitObject(repo, sha)
					}
					commit := parseCommit(obj.Data)
					sha = commit.Tree
				}
				if path == "" {
					return wrapGitObject(repo, sha)
				}
				cur := sha
				for _, part := range strings.Split(path, "/") {
					if part == "" {
						continue
					}
					o, oOk := repo.Objects[cur]
					if !oOk || o.Type != ObjTree {
						return nil
					}
					entries := parseTree(o.Data)
					found := false
					for _, e := range entries {
						if e.Name == part {
							cur = e.Sha
							found = true
							break
						}
					}
					if !found {
						return nil
					}
				}
				return wrapGitObject(repo, cur)
			},
		},
		"watchers": {
			Type: "UserConnection!",
			Args: CONN_ARGS,
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return gqlPaginate(nil, args)
			},
		},
		"stargazers": {
			Type: "StargazerConnection!",
			Args: CONN_ARGS,
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return gqlPaginate(nil, args)
			},
		},
	}
	S("Repository", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id": {
				Type: "ID!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return repoNodeId(repoFrom(s))
				},
			},
			"databaseId": {
				Type: "Int",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return repoFrom(s).ID
				},
			},
			"name": {
				Type: "String!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return repoFrom(s).Name
				},
			},
			"nameWithOwner": {
				Type: "String!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return ViewerLogin + "/" + repoFrom(s).Name
				},
			},
			"owner": {
				Type: "RepositoryOwner!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return wrapUser()
				},
			},
			"description": {
				Type: "String",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return repoFrom(s).Description
				},
			},
			"url": {
				Type: "URI!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return repoHtmlUrl(repoFrom(s))
				},
			},
			"homepageUrl": {
				Type: "URI",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return nil
				},
			},
			"isPrivate": {
				Type: "Boolean!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return repoFrom(s).IsPrivate
				},
			},
			"isFork": {
				Type: "Boolean!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return false
				},
			},
			"isArchived": {
				Type: "Boolean!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return false
				},
			},
			"isTemplate": {
				Type: "Boolean!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return false
				},
			},
			"isEmpty": {
				Type: "Boolean!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return len(repoFrom(s).Refs) == 0
				},
			},
			"createdAt": {
				Type: "DateTime!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return isoOf(EpochSec + int64(repoFrom(s).CreatedSeq))
				},
			},
			"updatedAt": {
				Type: "DateTime!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return isoOf(EpochSec + int64(repoFrom(s).CreatedSeq))
				},
			},
			"pushedAt": {
				Type: "DateTime",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return isoOf(EpochSec + int64(repoFrom(s).CreatedSeq))
				},
			},
			"defaultBranchRef": {
				Type: "Ref",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					repo := repoFrom(s)
					if repo == nil {
						return nil
					}
					name := "refs/heads/" + repo.DefaultBranch
					if _, ok := repo.Refs[name]; ok {
						return map[string]any{
							"__gqlType": "Ref",
							"repo":      repo,
							"name":      name,
						}
					}
					return nil
				},
			},
		},
	})
	if repoDef, ok := schema["Repository"]; ok && repoDef.Fields != nil {
		for k, v := range repoExtraFields {
			repoDef.Fields[k] = v
		}
	} else {
		S("Repository", GqlTypeDef{
			Kind:   "OBJECT",
			Fields: repoExtraFields,
		})
	}

	S("UserConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("User")})
	S("UserEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("User")})
	S("StargazerConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("User")})
	S("StargazerEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("User")})

	searchConnFields := connFields("SearchResultItem")
	searchConnFields["issueCount"] = GqlFieldDef{
		Type: "Int!",
		Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
			if m, ok := s.(map[string]any); ok {
				if counts, ok := m["counts"].(map[string]int); ok {
					return counts["issue"]
				}
				if counts, ok := m["counts"].(map[string]any); ok {
					switch v := counts["issue"].(type) {
					case int:
						return v
					case float64:
						return int(v)
					}
				}
			}
			return 0
		},
	}
	searchConnFields["repositoryCount"] = GqlFieldDef{
		Type: "Int!",
		Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
			if m, ok := s.(map[string]any); ok {
				if counts, ok := m["counts"].(map[string]int); ok {
					return counts["repository"]
				}
				if counts, ok := m["counts"].(map[string]any); ok {
					switch v := counts["repository"].(type) {
					case int:
						return v
					case float64:
						return int(v)
					}
				}
			}
			return 0
		},
	}
	searchConnFields["userCount"] = GqlFieldDef{
		Type:    "Int!",
		Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return 0 },
	}
	searchConnFields["wikiCount"] = GqlFieldDef{
		Type:    "Int!",
		Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return 0 },
	}
	searchConnFields["codeCount"] = GqlFieldDef{
		Type:    "Int!",
		Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return 0 },
	}
	searchConnFields["discussionCount"] = GqlFieldDef{
		Type:    "Int!",
		Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return 0 },
	}
	S("SearchResultItemConnection", GqlTypeDef{
		Kind:   "OBJECT",
		Fields: searchConnFields,
	})

	S("SearchResultItemEdge", GqlTypeDef{
		Kind:   "OBJECT",
		Fields: edgeFields("SearchResultItem"),
	})

	S("SearchResultItem", GqlTypeDef{
		Kind:          "UNION",
		PossibleTypes: []string{"Issue", "PullRequest", "Repository", "User"},
		ResolveType: func(s any) string {
			if m, ok := s.(map[string]any); ok {
				if t, ok := m["__gqlType"].(string); ok {
					return t
				}
			}
			return ""
		},
	})

	S("RateLimit", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"limit": {
				Type: "Int!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return 5000
				},
			},
			"cost": {
				Type: "Int!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return 1
				},
			},
			"remaining": {
				Type: "Int!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return 4999
				},
			},
			"used": {
				Type: "Int!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return 1
				},
			},
			"resetAt": {
				Type: "DateTime!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return isoOf(EpochSec + 3600)
				},
			},
			"nodeCount": {
				Type: "Int!",
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					return 0
				},
			},
		},
	})

	S("IssueConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("Issue")})
	S("IssueEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("Issue")})
	S("PullRequestConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("PullRequest")})
	S("PullRequestEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("PullRequest")})

	if queryDef, ok := schema["Query"]; ok && queryDef.Fields != nil {
		queryDef.Fields["viewer"] = GqlFieldDef{
			Type: "User!",
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return wrapUser()
			},
		}
		queryDef.Fields["repository"] = GqlFieldDef{
			Type: "Repository",
			Args: map[string]string{
				"owner": "String!",
				"name":  "String!",
			},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := lookupRepo(fmt.Sprint(args["owner"]), fmt.Sprint(args["name"]))
				if repo != nil {
					return wrapRepo(repo)
				}
				return nil
			},
		}
	} else {
		S("Query", GqlTypeDef{
			Kind: "OBJECT",
			Fields: map[string]GqlFieldDef{
				"viewer": {
					Type: "User!",
					Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
						return wrapUser()
					},
				},
				"repository": {
					Type: "Repository",
					Args: map[string]string{
						"owner": "String!",
						"name":  "String!",
					},
					Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
						repo := lookupRepo(fmt.Sprint(args["owner"]), fmt.Sprint(args["name"]))
						if repo != nil {
							return wrapRepo(repo)
						}
						return nil
					},
				},
			},
		})
	}

	if queryDef, ok := schema["Query"]; ok && queryDef.Fields != nil {
		queryDef.Fields["node"] = GqlFieldDef{
			Type: "Node",
			Args: map[string]string{"id": "ID!"},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return decodeNodeId(fmt.Sprint(args["id"]))
			},
		}
		queryDef.Fields["nodes"] = GqlFieldDef{
			Type: "[Node]!",
			Args: map[string]string{"ids": "[ID!]!"},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				ids, ok := args["ids"].([]any)
				if !ok {
					return []any{}
				}
				out := make([]any, len(ids))
				for i, id := range ids {
					out[i] = decodeNodeId(fmt.Sprint(id))
				}
				return out
			},
		}
		queryDef.Fields["rateLimit"] = GqlFieldDef{
			Type: "RateLimit",
			Args: map[string]string{"dryRun": "Boolean"},
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return map[string]any{"__gqlType": "RateLimit"}
			},
		}
		queryDef.Fields["search"] = GqlFieldDef{
			Type: "SearchResultItemConnection!",
			Args: mergeArgs(CONN_ARGS, map[string]string{"query": "String!", "type": "SearchType!"}),
			Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				q, _ := args["query"].(string)
				searchType, _ := args["type"].(string)
				if searchType == "" {
					searchType = "ISSUE"
				}
				var repoFilter *Repo
				terms := []string{}
				for _, tok := range strings.Fields(q) {
					if strings.HasPrefix(tok, "repo:") {
						parts := strings.Split(tok[len("repo:"):], "/")
						if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
							repoFilter = lookupRepo(parts[0], parts[1])
						}
					} else if strings.HasPrefix(tok, "type:") || strings.HasPrefix(tok, "in:") || strings.HasPrefix(tok, "is:") || strings.HasPrefix(tok, "state:") {
						continue
					} else {
						terms = append(terms, strings.ToLower(tok))
					}
				}
				matches := func(text string) bool {
					lower := strings.ToLower(text)
					for _, t := range terms {
						if !strings.Contains(lower, t) {
							return false
						}
					}
					return true
				}
				items := []any{}
				pool := []*Repo{}
				if repoFilter != nil {
					pool = append(pool, repoFilter)
				} else {
					for _, r := range repos {
						pool = append(pool, r)
					}
					// TS iterates repos.values() in insertion (creation) order;
					// sort the pool so result order — and cursor pagination over
					// it — is deterministic instead of Go map-iteration order.
					sort.Slice(pool, func(i, j int) bool {
						if pool[i].CreatedSeq != pool[j].CreatedSeq {
							return pool[i].CreatedSeq < pool[j].CreatedSeq
						}
						return pool[i].ID < pool[j].ID
					})
				}
				switch searchType {
				case "REPOSITORY":
					for _, r := range pool {
						if matches(r.Name) {
							items = append(items, wrapRepo(r))
						}
					}
				case "ISSUE":
					for _, r := range pool {
						for _, rec := range r.Issues {
							if rec.Kind == "issue" && matches(rec.Title) {
								items = append(items, wrapIssue(r, rec))
							}
						}
					}
				case "PULL_REQUEST":
					for _, r := range pool {
						for _, rec := range r.Issues {
							if rec.Kind == "pull" && matches(rec.Title) {
								items = append(items, wrapPr(r, rec))
							}
						}
					}
				case "USER":
					items = append(items, wrapUser())
				}
				page := gqlPaginate(items, args)
				issueCount, repositoryCount := 0, 0
				if searchType == "ISSUE" {
					issueCount = len(items)
				}
				if searchType == "REPOSITORY" {
					repositoryCount = len(items)
				}
				page["counts"] = map[string]int{"issue": issueCount, "repository": repositoryCount}
				return page
			},
		}
	}

	input := func(name string, fields map[string]string) {
		S(name, GqlTypeDef{Kind: "INPUT_OBJECT", InputFields: fields})
	}
	input("CreateIssueInput", map[string]string{"repositoryId": "ID!", "title": "String!", "body": "String", "clientMutationId": "String"})
	input("CloseIssueInput", map[string]string{"issueId": "ID!", "stateReason": "IssueClosedStateReason", "clientMutationId": "String"})
	input("CreatePullRequestInput", map[string]string{"repositoryId": "ID!", "baseRefName": "String!", "headRefName": "String!", "title": "String!", "body": "String", "draft": "Boolean", "clientMutationId": "String"})
	input("MergePullRequestInput", map[string]string{"pullRequestId": "ID!", "mergeMethod": "PullRequestMergeMethod", "commitHeadline": "String", "clientMutationId": "String"})
	input("AddCommentInput", map[string]string{"subjectId": "ID!", "body": "String!", "clientMutationId": "String"})
	input("IssueOrder", map[string]string{"field": "IssueOrderField!", "direction": "OrderDirection!"})
	input("IssueFilters", map[string]string{"states": "[IssueState!]", "labels": "[String!]", "since": "DateTime"})
	input("RepositoryOrder", map[string]string{"field": "RepositoryOrderField!", "direction": "OrderDirection!"})

	S("CreateIssuePayload", GqlTypeDef{Kind: "OBJECT", Fields: map[string]GqlFieldDef{
		"issue":            {Type: "Issue", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["issue"] }},
		"clientMutationId": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["clientMutationId"] }},
	}})
	S("CloseIssuePayload", GqlTypeDef{Kind: "OBJECT", Fields: map[string]GqlFieldDef{
		"issue":            {Type: "Issue", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["issue"] }},
		"clientMutationId": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["clientMutationId"] }},
	}})
	S("CreatePullRequestPayload", GqlTypeDef{Kind: "OBJECT", Fields: map[string]GqlFieldDef{
		"pullRequest":      {Type: "PullRequest", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["pullRequest"] }},
		"clientMutationId": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["clientMutationId"] }},
	}})
	S("MergePullRequestPayload", GqlTypeDef{Kind: "OBJECT", Fields: map[string]GqlFieldDef{
		"pullRequest":      {Type: "PullRequest", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["pullRequest"] }},
		"clientMutationId": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["clientMutationId"] }},
	}})
	S("AddCommentPayload", GqlTypeDef{Kind: "OBJECT", Fields: map[string]GqlFieldDef{
		"commentEdge":      {Type: "IssueCommentEdge", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["commentEdge"] }},
		"subject":          {Type: "Node", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["subject"] }},
		"clientMutationId": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["clientMutationId"] }},
	}})

	inpOf := func(args map[string]any) map[string]any {
		inp, _ := args["input"].(map[string]any)
		if inp == nil {
			inp = map[string]any{}
		}
		return inp
	}
	strField := func(m map[string]any, key string) string {
		if v, ok := m[key].(string); ok {
			return v
		}
		return ""
	}

	S("Mutation", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"createIssue": {
				Type: "CreateIssuePayload",
				Args: map[string]string{"input": "CreateIssueInput!"},
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					inp := inpOf(args)
					target, _ := decodeNodeId(strField(inp, "repositoryId")).(map[string]any)
					if target == nil || target["__gqlType"] != "Repository" {
						panic("repository not found")
					}
					repo := target["repo"].(*Repo)
					rec := &IssueRec{
						Kind:       "issue",
						Number:     repo.NextNumber,
						ID:         nextIssueID,
						Title:      strField(inp, "title"),
						Body:       strField(inp, "body"),
						State:      "open",
						Labels:     []string{},
						Comments:   []Comment{},
						CreatedSeq: seqCounter + 1,
						Ver:        0,
					}
					repo.NextNumber++
					nextIssueID++
					seqCounter++
					repo.Issues = append(repo.Issues, rec)
					return map[string]any{"__gqlType": "CreateIssuePayload", "issue": wrapIssue(repo, rec), "clientMutationId": inp["clientMutationId"]}
				},
			},
			"closeIssue": {
				Type: "CloseIssuePayload",
				Args: map[string]string{"input": "CloseIssueInput!"},
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					inp := inpOf(args)
					target, _ := decodeNodeId(strField(inp, "issueId")).(map[string]any)
					if target == nil || target["__gqlType"] != "Issue" {
						panic("issue not found")
					}
					rec := target["rec"].(*IssueRec)
					rec.State = "closed"
					rec.Ver++
					return map[string]any{"__gqlType": "CloseIssuePayload", "issue": target, "clientMutationId": inp["clientMutationId"]}
				},
			},
			"createPullRequest": {
				Type: "CreatePullRequestPayload",
				Args: map[string]string{"input": "CreatePullRequestInput!"},
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					inp := inpOf(args)
					target, _ := decodeNodeId(strField(inp, "repositoryId")).(map[string]any)
					if target == nil || target["__gqlType"] != "Repository" {
						panic("repository not found")
					}
					repo := target["repo"].(*Repo)
					headRef := strField(inp, "headRefName")
					baseRef := strField(inp, "baseRefName")
					if branchTip(repo, headRef) == "" {
						panic(fmt.Sprintf("head ref %s does not exist", headRef))
					}
					if branchTip(repo, baseRef) == "" {
						panic(fmt.Sprintf("base ref %s does not exist", baseRef))
					}
					rec := &IssueRec{
						Kind:       "pull",
						Number:     repo.NextNumber,
						ID:         nextIssueID,
						Title:      strField(inp, "title"),
						Body:       strField(inp, "body"),
						State:      "open",
						Labels:     []string{},
						Comments:   []Comment{},
						CreatedSeq: seqCounter + 1,
						Ver:        0,
						HeadRef:    headRef,
						BaseRef:    baseRef,
						Merged:     false,
					}
					repo.NextNumber++
					nextIssueID++
					seqCounter++
					repo.Issues = append(repo.Issues, rec)
					return map[string]any{"__gqlType": "CreatePullRequestPayload", "pullRequest": wrapPr(repo, rec), "clientMutationId": inp["clientMutationId"]}
				},
			},
			"mergePullRequest": {
				Type: "MergePullRequestPayload",
				Args: map[string]string{"input": "MergePullRequestInput!"},
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					inp := inpOf(args)
					target, _ := decodeNodeId(strField(inp, "pullRequestId")).(map[string]any)
					if target == nil || target["__gqlType"] != "PullRequest" {
						panic("pull request not found")
					}
					sha := mergePullRec(target["repo"].(*Repo), target["rec"].(*IssueRec))
					if sha == nil {
						panic("pull request is not mergeable")
					}
					return map[string]any{"__gqlType": "MergePullRequestPayload", "pullRequest": target, "clientMutationId": inp["clientMutationId"]}
				},
			},
			"addComment": {
				Type: "AddCommentPayload",
				Args: map[string]string{"input": "AddCommentInput!"},
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					inp := inpOf(args)
					target, _ := decodeNodeId(strField(inp, "subjectId")).(map[string]any)
					if target == nil || (target["__gqlType"] != "Issue" && target["__gqlType"] != "PullRequest") {
						panic("subject not found")
					}
					repo := target["repo"].(*Repo)
					rec := target["rec"].(*IssueRec)
					comment := Comment{ID: nextIssueID, Body: strField(inp, "body")}
					nextIssueID++
					rec.Comments = append(rec.Comments, comment)
					rec.Ver++
					wrapped := wrapComment(repo, rec, comment)
					return map[string]any{
						"__gqlType":        "AddCommentPayload",
						"commentEdge":      map[string]any{"node": wrapped, "cursor": b64e(fmt.Sprintf("cursor:%d", len(rec.Comments)-1))},
						"subject":          target,
						"clientMutationId": inp["clientMutationId"],
					}
				},
			},
		},
	})

	S("IssueState", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"OPEN", "CLOSED"}})
	S("IssueStateReason", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"COMPLETED", "NOT_PLANNED", "REOPENED"}})
	S("IssueClosedStateReason", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"COMPLETED", "NOT_PLANNED"}})
	S("PullRequestState", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"OPEN", "CLOSED", "MERGED"}})
	S("PullRequestMergeMethod", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"MERGE", "SQUASH", "REBASE"}})
	S("MergeableState", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"CONFLICTING", "MERGEABLE", "UNKNOWN"}})
	S("OrderDirection", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"ASC", "DESC"}})
	S("IssueOrderField", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}})
	S("RepositoryOrderField", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"CREATED_AT", "UPDATED_AT", "PUSHED_AT", "NAME", "STARGAZERS"}})
	S("SearchType", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}})
	S("RepositoryPermission", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"ADMIN", "MAINTAIN", "READ", "TRIAGE", "WRITE"}})
	S("PatchStatus", GqlTypeDef{Kind: "ENUM", EnumValues: []string{"ADDED", "CHANGED", "COPIED", "DELETED", "MODIFIED", "RENAMED"}})

	S("Language", GqlTypeDef{Kind: "OBJECT", Fields: map[string]GqlFieldDef{
		"id":    {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }},
		"name":  {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }},
		"color": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return nil }},
	}})
	S("License", GqlTypeDef{Kind: "OBJECT", Fields: map[string]GqlFieldDef{
		"id":     {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }},
		"name":   {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }},
		"spdxId": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return nil }},
		"key":    {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }},
	}})

	S("Issue", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id":       {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return issueNodeId(repoFrom(s), recOf(s)) }},
			"number":   {Type: "Int!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Number }},
			"title":    {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Title }},
			"body":     {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Body }},
			"bodyText": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Body }},
			"state":    {Type: "IssueState!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return strings.ToUpper(recOf(s).State) }},
			"stateReason": {Type: "IssueStateReason", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if recOf(s).State == "closed" {
					return "COMPLETED"
				}
				return nil
			}},
			"author": {Type: "Actor", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapUser() }},
			"labels": {
				Type: "LabelConnection!",
				Args: CONN_ARGS,
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					repo := repoFrom(s)
					rec := recOf(s)
					var nodes []any
					for _, name := range rec.Labels {
						nodes = append(nodes, wrapLabel(repo, name))
					}
					return gqlPaginate(nodes, args)
				},
			},
			"comments": {
				Type: "IssueCommentConnection!",
				Args: CONN_ARGS,
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					repo := repoFrom(s)
					rec := recOf(s)
					var nodes []any
					for _, c := range rec.Comments {
						nodes = append(nodes, wrapComment(repo, rec, c))
					}
					return gqlPaginate(nodes, args)
				},
			},
			"createdAt": {Type: "DateTime!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return tsOf(recOf(s)) }},
			"updatedAt": {Type: "DateTime!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return tsOf(recOf(s)) }},
			"closedAt":  {Type: "DateTime", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return nil }},
			"url": {Type: "URI!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return fmt.Sprintf("%s/issues/%d", repoHtmlUrl(repoFrom(s)), recOf(s).Number)
			}},
			"repository":      {Type: "Repository!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapRepo(repoFrom(s)) }},
			"viewerDidAuthor": {Type: "Boolean!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return true }},
		},
	})

	S("Label", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id":   {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return b64e("Label:" + nameOf(s)) }},
			"name": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return nameOf(s) }},
			"color": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if repo != nil {
					if l, ok := repo.Labels[nameOf(s)]; ok {
						return l.Color
					}
				}
				return "ededed"
			}},
			"description": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return nil }},
		},
	})
	S("LabelConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("Label")})
	S("LabelEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("Label")})

	S("IssueComment", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id": {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return commentNodeId(repoFrom(s), recOf(s), commentOf(s))
			}},
			"body":      {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return commentOf(s).Body }},
			"bodyText":  {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return commentOf(s).Body }},
			"author":    {Type: "Actor", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapUser() }},
			"createdAt": {Type: "DateTime!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return tsOf(recOf(s)) }},
			"updatedAt": {Type: "DateTime!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return tsOf(recOf(s)) }},
			"url": {Type: "URI!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return fmt.Sprintf("%s/issues/%d", repoHtmlUrl(repoFrom(s)), recOf(s).Number)
			}},
		},
	})
	S("IssueCommentConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("IssueComment")})
	S("IssueCommentEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("IssueComment")})

	S("PullRequest", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id":       {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return prNodeId(repoFrom(s), recOf(s)) }},
			"number":   {Type: "Int!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Number }},
			"title":    {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Title }},
			"body":     {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Body }},
			"bodyText": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Body }},
			"state": {Type: "PullRequestState!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				rec := recOf(s)
				if rec.Merged {
					return "MERGED"
				}
				return strings.ToUpper(rec.State)
			}},
			"merged": {Type: "Boolean!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).Merged }},
			"mergedAt": {Type: "DateTime", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				rec := recOf(s)
				if !rec.Merged {
					return nil
				}
				if rec.MergedTs != 0 {
					return isoOf(rec.MergedTs)
				}
				return isoOf(EpochSec)
			}},
			"closedAt": {Type: "DateTime", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				rec := recOf(s)
				if rec.State == "closed" {
					return tsOf(rec)
				}
				return nil
			}},
			"isDraft":     {Type: "Boolean!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return false }},
			"author":      {Type: "Actor", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapUser() }},
			"baseRefName": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).BaseRef }},
			"headRefName": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return recOf(s).HeadRef }},
			"baseRefOid": {Type: "GitObjectID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				rec := recOf(s)
				sha := ""
				if rec.BaseRef != "" {
					sha = branchTip(repoFrom(s), rec.BaseRef)
				}
				if sha == "" {
					return ZERO_SHA
				}
				return sha
			}},
			"headRefOid": {Type: "GitObjectID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				rec := recOf(s)
				sha := ""
				if rec.HeadRef != "" {
					sha = branchTip(repoFrom(s), rec.HeadRef)
				}
				if sha == "" {
					return ZERO_SHA
				}
				return sha
			}},
			"mergeable": {Type: "MergeableState!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				rec := recOf(s)
				if rec.Merged || rec.State == "closed" {
					return "UNKNOWN"
				}
				var headSha, baseSha string
				if rec.HeadRef != "" {
					headSha = branchTip(repo, rec.HeadRef)
				}
				if rec.BaseRef != "" {
					baseSha = branchTip(repo, rec.BaseRef)
				}
				if headSha == "" || baseSha == "" {
					return "UNKNOWN"
				}
				if _, ok := mergeTrees(repo, baseSha, headSha); ok {
					return "MERGEABLE"
				}
				return "CONFLICTING"
			}},
			"createdAt": {Type: "DateTime!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return tsOf(recOf(s)) }},
			"updatedAt": {Type: "DateTime!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return tsOf(recOf(s)) }},
			"url": {Type: "URI!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return fmt.Sprintf("%s/pull/%d", repoHtmlUrl(repoFrom(s)), recOf(s).Number)
			}},
			"repository": {Type: "Repository!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapRepo(repoFrom(s)) }},
			"comments": {
				Type: "IssueCommentConnection!",
				Args: CONN_ARGS,
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					repo := repoFrom(s)
					rec := recOf(s)
					var nodes []any
					for _, c := range rec.Comments {
						nodes = append(nodes, wrapComment(repo, rec, c))
					}
					return gqlPaginate(nodes, args)
				},
			},
			"files": {
				Type: "PullRequestChangedFileConnection",
				Args: CONN_ARGS,
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					repo := repoFrom(s)
					rec := recOf(s)
					var nodes []any
					for _, f := range pullFiles(repo, rec) {
						status, _ := f["status"].(string)
						changeType := "MODIFIED"
						switch status {
						case "added":
							changeType = "ADDED"
						case "removed":
							changeType = "DELETED"
						case "renamed":
							changeType = "RENAMED"
						}
						nodes = append(nodes, map[string]any{
							"__gqlType":  "PullRequestChangedFile",
							"path":       f["filename"],
							"additions":  f["additions"],
							"deletions":  f["deletions"],
							"changeType": changeType,
						})
					}
					return gqlPaginate(nodes, args)
				},
			},
		},
	})
	S("PullRequestChangedFile", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"path":       {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["path"] }},
			"additions":  {Type: "Int!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["additions"] }},
			"deletions":  {Type: "Int!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["deletions"] }},
			"changeType": {Type: "PatchStatus!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["changeType"] }},
		},
	})
	S("PullRequestChangedFileConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("PullRequestChangedFile")})
	S("PullRequestChangedFileEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("PullRequestChangedFile")})
	S("IssueOrPullRequest", GqlTypeDef{Kind: "UNION", PossibleTypes: []string{"Issue", "PullRequest"}, ResolveType: func(s any) string { return s.(map[string]any)["__gqlType"].(string) }})

	S("Ref", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id": {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return b64e(fmt.Sprintf("Ref:%d:%s", repoFrom(s).ID, nameOf(s)))
			}},
			"name": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				name := nameOf(s)
				if strings.HasPrefix(name, "refs/heads/") {
					return name[len("refs/heads/"):]
				}
				if strings.HasPrefix(name, "refs/tags/") {
					return name[len("refs/tags/"):]
				}
				return name
			}},
			"prefix": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if strings.HasPrefix(nameOf(s), "refs/tags/") {
					return "refs/tags/"
				}
				return "refs/heads/"
			}},
			"qualifiedName": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return nameOf(s) }},
			"target": {Type: "GitObject", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				sha := repo.Refs[nameOf(s)]
				if sha == "" {
					sha = ZERO_SHA
				}
				return wrapGitObject(repo, sha)
			}},
			"repository": {Type: "Repository!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapRepo(repoFrom(s)) }},
		},
	})
	S("RefConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("Ref")})
	S("RefEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("Ref")})

	S("Commit", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id":             {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return b64e("Commit:" + shaOf(s)) }},
			"oid":            {Type: "GitObjectID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return shaOf(s) }},
			"abbreviatedOid": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return abbrevSha(shaOf(s)) }},
			"message": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if info, ok := commitInfoOf(repoFrom(s), shaOf(s)); ok {
					return strings.TrimSuffix(info.Message, "\n")
				}
				return ""
			}},
			"messageHeadline": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if info, ok := commitInfoOf(repoFrom(s), shaOf(s)); ok {
					return strings.SplitN(info.Message, "\n", 2)[0]
				}
				return ""
			}},
			"tree": {Type: "Tree!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				tree := ZERO_SHA
				if info, ok := commitInfoOf(repo, shaOf(s)); ok {
					tree = info.Tree
				}
				return map[string]any{"__gqlType": "Tree", "repo": repo, "sha": tree}
			}},
			"treeOid": {Type: "GitObjectID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if info, ok := commitInfoOf(repoFrom(s), shaOf(s)); ok {
					return info.Tree
				}
				return ZERO_SHA
			}},
			"committedDate": {Type: "GitTimestamp!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if info, ok := commitInfoOf(repoFrom(s), shaOf(s)); ok {
					return identParts(info.CommitterLine)["date"]
				}
				return isoOf(EpochSec)
			}},
			"authoredDate": {Type: "GitTimestamp!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if info, ok := commitInfoOf(repoFrom(s), shaOf(s)); ok {
					return identParts(info.AuthorLine)["date"]
				}
				return isoOf(EpochSec)
			}},
			"author": {Type: "GitActor", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if info, ok := commitInfoOf(repoFrom(s), shaOf(s)); ok {
					p := identParts(info.AuthorLine)
					return wrapGitActor(fmt.Sprint(p["name"]), fmt.Sprint(p["email"]), fmt.Sprint(p["date"]))
				}
				return wrapGitActor("", "", isoOf(EpochSec))
			}},
			"committer": {Type: "GitActor", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				if info, ok := commitInfoOf(repoFrom(s), shaOf(s)); ok {
					p := identParts(info.CommitterLine)
					return wrapGitActor(fmt.Sprint(p["name"]), fmt.Sprint(p["email"]), fmt.Sprint(p["date"]))
				}
				return wrapGitActor("", "", isoOf(EpochSec))
			}},
			"parents": {
				Type: "CommitConnection!",
				Args: CONN_ARGS,
				Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
					repo := repoFrom(s)
					var nodes []any
					if info, ok := commitInfoOf(repo, shaOf(s)); ok {
						for _, p := range info.Parents {
							nodes = append(nodes, map[string]any{"__gqlType": "Commit", "repo": repo, "sha": p})
						}
					}
					return gqlPaginate(nodes, args)
				},
			},
			"url": {Type: "URI!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				return fmt.Sprintf("%s/commit/%s", repoHtmlUrl(repoFrom(s)), shaOf(s))
			}},
			"repository": {Type: "Repository!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapRepo(repoFrom(s)) }},
		},
	})
	S("CommitConnection", GqlTypeDef{Kind: "OBJECT", Fields: connFields("Commit")})
	S("CommitEdge", GqlTypeDef{Kind: "OBJECT", Fields: edgeFields("Commit")})

	S("GitActor", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"name":  {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["name"] }},
			"email": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["email"] }},
			"date":  {Type: "GitTimestamp", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["date"] }},
		},
	})

	S("Tree", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id":  {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return b64e("Tree:" + shaOf(s)) }},
			"oid": {Type: "GitObjectID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return shaOf(s) }},
			"entries": {Type: "[TreeEntry!]!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				nodes := []any{}
				if obj, ok := repo.Objects[shaOf(s)]; ok {
					for _, e := range parseTree(obj.Data) {
						t := "blob"
						if e.Mode == "40000" {
							t = "tree"
						}
						nodes = append(nodes, map[string]any{"__gqlType": "TreeEntry", "repo": repo, "name": e.Name, "sha": e.Sha, "mode": e.Mode, "type": t})
					}
				}
				return nodes
			}},
			"repository": {Type: "Repository!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapRepo(repoFrom(s)) }},
		},
	})

	S("TreeEntry", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"name": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return nameOf(s) }},
			"path": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return nameOf(s) }},
			"mode": {Type: "Int!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				v, err := strconv.ParseInt(fmt.Sprint(s.(map[string]any)["mode"]), 8, 64)
				if err != nil {
					return 0
				}
				return int(v)
			}},
			"type":   {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return s.(map[string]any)["type"] }},
			"object": {Type: "GitObject", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return wrapGitObject(repoFrom(s), shaOf(s)) }},
		},
	})

	S("Blob", GqlTypeDef{
		Kind: "OBJECT",
		Fields: map[string]GqlFieldDef{
			"id":  {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return b64e("Blob:" + shaOf(s)) }},
			"oid": {Type: "GitObjectID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return shaOf(s) }},
			"text": {Type: "String", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if obj, ok := repo.Objects[shaOf(s)]; ok {
					return string(obj.Data)
				}
				return nil
			}},
			"byteSize": {Type: "Int!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any {
				repo := repoFrom(s)
				if obj, ok := repo.Objects[shaOf(s)]; ok {
					return len(obj.Data)
				}
				return 0
			}},
			"isTruncated": {Type: "Boolean!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return false }},
			"isBinary":    {Type: "Boolean", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return false }},
		},
	})

	S("GitObject", GqlTypeDef{
		Kind:          "INTERFACE",
		PossibleTypes: []string{"Commit", "Tree", "Blob"},
		ResolveType:   func(s any) string { return s.(map[string]any)["__gqlType"].(string) },
		Fields: map[string]GqlFieldDef{
			"id":             {Type: "ID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return "" }},
			"oid":            {Type: "GitObjectID!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return shaOf(s) }},
			"abbreviatedOid": {Type: "String!", Resolve: func(s any, args map[string]any, ctx *GqlCtx) any { return abbrevSha(shaOf(s)) }},
		},
	})

	buildIntrospection(schema)
	return schema
}

var FORGE_SCHEMA = buildForgeSchema()
