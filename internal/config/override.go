package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/nickstrad/kb/internal/errs"
)

// Apply sets one key of c from a "a.b.c=value" assignment, the form --set takes, and returns the
// new Config; c is not modified. The path follows the TOML tables (search.bm25.title=4,
// search.rrf_k=30). The value is a TOML literal: 4, 1.5, true, "x". A string
// key also takes a bare word (embed.model=none), and a list key takes a bare comma-separated
// list (search.stores=global,notes). The result is validated like a file. Every failure is a
// Usage error naming the assignment.
func Apply(c Config, assignment string) (Config, error) { return ApplyAs(c, "--set", assignment) }

// ApplyAs is Apply for an assignment that came from another flag (kb eval --sweep): its errors
// name that flag instead of --set.
func ApplyAs(c Config, flag, assignment string) (Config, error) {
	fail := func(format string, args ...any) (Config, error) {
		return Config{}, errs.New(errs.Usage, "config.Apply", "%s %s: %s", flag, assignment, fmt.Sprintf(format, args...))
	}
	path, value, ok := strings.Cut(assignment, "=")
	path = strings.TrimSpace(path)
	if !ok || path == "" {
		return fail("want key=value, such as search.rrf_k=30")
	}
	value = strings.TrimSpace(value)

	segs := strings.Split(path, ".")
	tgt, err := resolve(reflect.TypeOf(Config{}), segs)
	if err != nil {
		return fail("%v", err)
	}

	lit, err := literal(tgt.kind, value)
	if err != nil {
		return fail("%v", err)
	}
	doc := tgt.table + tomlKey(tgt.key) + " = " + lit + "\n"

	next := clone(c)
	md, err := toml.Decode(doc, &next)
	if err != nil {
		var pe toml.ParseError
		msg := err.Error()
		if errors.As(err, &pe) {
			msg = pe.Message
		} else if m := typeErrRE.FindStringSubmatch(msg); m != nil {
			msg = m[3] // a type mismatch: drop the decoder's own "toml: line N (last key ...)" prefix
		}
		return fail("%s", msg)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return fail("unknown key %q", path)
	}
	normalize(&next)
	if err := next.Validate(flag + " " + assignment); err != nil {
		return Config{}, err
	}
	return next, nil
}

// target is where an assignment lands: the TOML table header to write, the key in it, and the
// kind of value the key holds.
type target struct {
	table string // "[search.bm25]\n", or "" for a top-level key
	key   string
	kind  reflect.Kind
}

// resolve walks the Config type along segs by toml tags.
func resolve(t reflect.Type, segs []string) (target, error) {
	var tables []string
	for i := 0; i < len(segs); i++ {
		if t.Kind() != reflect.Struct {
			return target{}, fmt.Errorf("%q is a value, not a table", strings.Join(segs[:i], "."))
		}
		f, ok := fieldByTag(t, segs[i])
		if !ok {
			return target{}, fmt.Errorf("unknown key %q", strings.Join(segs[:i+1], "."))
		}
		tables = append(tables, segs[i])
		t = f.Type
	}
	switch t.Kind() {
	case reflect.Struct:
		return target{}, fmt.Errorf("%q is a table; set one of its keys, such as %s.%s", strings.Join(segs, "."), strings.Join(segs, "."), firstTag(t))
	case reflect.Slice:
		if t.Elem().Kind() != reflect.String {
			return target{}, fmt.Errorf("%q cannot be set with --set; edit the config file", strings.Join(segs, "."))
		}
	}
	return finish(tables[:len(tables)-1], tables[len(tables)-1], t.Kind()), nil
}

// finish builds the target for a key under the given table path.
func finish(tables []string, key string, kind reflect.Kind) target {
	tg := target{key: key, kind: kind}
	if len(tables) > 0 {
		parts := make([]string, len(tables))
		for i, p := range tables {
			parts[i] = tomlKey(p)
		}
		tg.table = "[" + strings.Join(parts, ".") + "]\n"
	}
	return tg
}

func fieldByTag(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("toml") == name {
			return t.Field(i), true
		}
	}
	return reflect.StructField{}, false
}

func firstTag(t reflect.Type) string {
	if t.NumField() == 0 {
		return ""
	}
	return t.Field(0).Tag.Get("toml")
}

// literal renders value as a TOML literal for a key of the given kind.
func literal(kind reflect.Kind, value string) (string, error) {
	switch kind {
	case reflect.String:
		if strings.HasPrefix(value, `"`) {
			return value, nil
		}
		return quote(value), nil
	case reflect.Slice:
		if strings.HasPrefix(value, "[") {
			return value, nil
		}
		var items []string
		for _, p := range strings.Split(value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				items = append(items, quote(p))
			}
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	}
	if value == "" {
		return "", fmt.Errorf("missing value")
	}
	return value, nil
}

// quote renders s as a TOML basic string.
func quote(s string) string {
	var buf bytes.Buffer
	// The encoder escapes control characters and quotes; a map with one string value is the
	// smallest document it can write.
	if err := toml.NewEncoder(&buf).Encode(map[string]string{"v": s}); err != nil {
		return `""`
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(buf.String()), "v = "))
}

// tomlKey quotes a key unless it is a bare TOML key.
func tomlKey(k string) string {
	if k != "" && strings.Trim(k, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == "" {
		return k
	}
	return quote(k)
}

// clone copies c deeply enough that decoding onto the copy leaves c alone.
func clone(c Config) Config {
	out := c
	out.Sources = make([]Source, len(c.Sources))
	for i, s := range c.Sources {
		s.Exclude = append([]string{}, s.Exclude...)
		out.Sources[i] = s
	}
	out.Search.Stores = append([]string{}, c.Search.Stores...)
	return out
}

// EffectiveJSON is the canonical form of the settings that decide what a search returns and what
// the index holds, [embed] and [search]: compact JSON with every object's keys sorted. hash is the
// hex SHA-256 of those bytes, the settings_hash a search log records. Equal settings give equal
// bytes whatever order the file listed them in.
func (c Config) EffectiveJSON() (data []byte, hash string, err error) {
	raw, err := json.Marshal(struct {
		Embed  Embed  `json:"embed"`
		Search Search `json:"search"`
	}{c.Embed, c.Search})
	if err != nil {
		return nil, "", errs.Wrap(err, errs.Internal, "config.EffectiveJSON", "cannot encode the effective settings")
	}
	// A round trip through a generic value sorts the keys; UseNumber keeps numbers as written.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, "", errs.Wrap(err, errs.Internal, "config.EffectiveJSON", "cannot encode the effective settings")
	}
	data, err = json.Marshal(generic)
	if err != nil {
		return nil, "", errs.Wrap(err, errs.Internal, "config.EffectiveJSON", "cannot encode the effective settings")
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}
