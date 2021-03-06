// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package extraconfig

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
)

const (
	// DefaultTagName value
	DefaultTagName = "vic"
	// DefaultPrefix value
	DefaultPrefix = ""
	// DefaultGuestInfoPrefix value
	DefaultGuestInfoPrefix = "guestinfo.vice."
)

const (
	// Invalid value
	Invalid = 1 << iota
	// Hidden value
	Hidden
	// ReadOnly value
	ReadOnly
	// ReadWrite value
	ReadWrite
	// NonPersistent value
	NonPersistent
	// Volatile value
	Volatile
	// Secret value
	Secret
)

type recursion struct {
	// depth is a recursion depth, 0 equating to skip field
	depth int
	// follow controls whether we follow pointers
	follow bool
}

// Unbounded is the value used for unbounded recursion
var Unbounded = recursion{depth: -1, follow: true}

// calculateScope returns the uint representation of scope tag
func calculateScope(scopes []string) uint {
	var scope uint
	if len(scopes) == 0 {
		return Hidden | ReadOnly
	}

	for _, v := range scopes {
		switch v {
		case "hidden":
			scope |= Hidden
		case "read-only":
			scope |= ReadOnly
		case "read-write":
			scope |= ReadWrite
		case "non-persistent":
			scope |= NonPersistent
		case "volatile":
			scope |= Volatile
		case "secret":
			scope |= Secret | ReadOnly
		default:
			return Invalid
		}
	}
	return scope
}

func isSecret(key string) bool {
	return strings.HasSuffix(key, "@secret")
}

func calculateScopeFromKey(key string) []string {
	scopes := []string{}

	if !strings.HasPrefix(key, DefaultGuestInfoPrefix) {
		scopes = append(scopes, "hidden")
	}

	if strings.Contains(key, "/") {
		scopes = append(scopes, "read-only")
	} else {
		scopes = append(scopes, "read-write")
	}

	if isSecret(key) {
		scopes = append(scopes, "secret")
	}

	return scopes
}

func calculateKeyFromField(field reflect.StructField, prefix string, depth recursion) (string, recursion) {
	skip := recursion{}
	//skip unexported fields
	if field.PkgPath != "" {
		log.Debugf("Skipping %s (not exported)", field.Name)
		return "", skip
	}

	// get the annotations
	tags := field.Tag
	log.Debugf("Tags: %#v", tags)

	var key string
	var scopes []string

	fdepth := depth

	// do we have DefaultTagName?
	if tags.Get(DefaultTagName) != "" {
		// get the scopes
		scopes = strings.Split(tags.Get("scope"), ",")
		log.Debugf("Scopes: %#v", scopes)

		// get the keys and split properties from it
		key = tags.Get("key")
		log.Debugf("Key specified: %s", key)

		// get the keys and split properties from it
		recurse := tags.Get("recurse")
		if recurse != "" {
			props := strings.Split(recurse, ",")
			// process properties
			for _, prop := range props {
				// determine recursion depth
				if strings.HasPrefix(prop, "depth") {
					parts := strings.Split(prop, "=")
					if len(parts) != 2 {
						log.Warnf("Skipping field with incorrect recurse property: %s", prop)
						return "", skip
					}

					val, err := strconv.ParseInt(parts[1], 10, 64)
					if err != nil {
						log.Warnf("Skipping field with incorrect recurse value: %s", parts[1])
						return "", skip
					}
					fdepth.depth = int(val)
				} else if prop == "nofollow" {
					fdepth.follow = false
				} else if prop == "follow" {
					fdepth.follow = true
				} else {
					log.Warnf("Ignoring unknown recurse property %s (%s)", key, prop)
					continue
				}
			}
		}
	} else {
		log.Debugf("%s not tagged - inheriting parent scope", field.Name)
		scopes = calculateScopeFromKey(prefix)
	}

	if key == "" {
		log.Debugf("%s does not specify key - defaulting to fieldname", field.Name)
		key = field.Name
	}

	// re-calculate the key based on the scope and prefix
	if key = calculateKey(scopes, prefix, key); key == "" {
		log.Warnf("Skipping %s (unknown scope %s)", field.Name, scopes)
		return "", skip
	}

	return key, fdepth
}

// calculateKey calculates the key based on the scope and current prefix
func calculateKey(scopes []string, prefix string, key string) string {
	scope := calculateScope(scopes)
	if scope&Invalid != 0 || scope&NonPersistent != 0 {
		log.Debugf("invalid scope")
		return ""
	}

	newSep := "/"
	oldSep := "."
	key = strings.TrimSpace(key)

	hide := scope&Hidden != 0
	write := scope&ReadWrite != 0
	visible := strings.HasPrefix(prefix, DefaultGuestInfoPrefix)

	if !hide && write {
		oldSep = "/"
		newSep = "."
	}

	// assemble the actual keypath with appropriate separators
	out := key
	if prefix != "" {
		out = strings.Join([]string{prefix, key}, newSep)
	}

	if scope&Secret != 0 {
		out += "@secret"
	}

	// we don't care about existing separators when hiden
	if hide {
		if !visible {
			return out
		}

		// strip the prefix and the leading r/w signifier
		return out[len(DefaultGuestInfoPrefix)+1:]
	}

	// ensure that separators are correct
	out = strings.Replace(out, oldSep, newSep, -1)

	// Assemble the base that controls key publishing in guest
	if !visible {
		return DefaultGuestInfoPrefix + newSep + out
	}

	// prefix will have been mangled by strings.Replace
	return DefaultGuestInfoPrefix + out[len(DefaultGuestInfoPrefix):]
}

// utility function to allow adding of arbitrary prefix into key
// header is a leading segment that is preserved, prefix is injected after that
func addPrefixToKey(header, prefix, key string) string {
	if prefix == "" {
		return key
	}

	base := strings.TrimPrefix(key, header)
	separator := base[0]

	var modifiedPrefix string
	if separator == '.' {
		modifiedPrefix = strings.Replace(prefix, "/", ".", -1)
	} else {
		modifiedPrefix = strings.Replace(prefix, ".", "/", -1)
	}

	// we assume (given usage comment for WithPrefix) that there's no leading or trailing separator
	// on the prefix. base has a leading separator
	// guestinfoPrefix is const so adding it to the format string directly
	return fmt.Sprintf(header+"%c%s%s", separator, modifiedPrefix, base)
}

func calculateKeys(v reflect.Value, field string, prefix string) []string {
	log.Debugf("v=%#v, field=%#v, prefix=%#v", v, field, prefix)
	if v.Kind() == reflect.Ptr {
		return calculateKeys(v.Elem(), field, prefix)
	}

	if field == "" {
		return []string{prefix}
	}

	s := strings.SplitN(field, ".", 2)
	field = ""
	iterate := false
	if s[0] == "*" {
		iterate = true
	}

	if len(s) > 1 {
		field = s[1]
	}

	if !iterate {
		switch v.Kind() {
		case reflect.Map:
			found := false
			for _, k := range v.MapKeys() {
				sk := k.Convert(reflect.TypeOf(""))
				if sk.String() == s[0] {
					v = v.MapIndex(k)
					found = true
					break
				}
			}

			if !found {
				panic(fmt.Sprintf("could not find map key %s", s[0]))
			}
			prefix = fmt.Sprintf("%s|%s", prefix, s[0])
		case reflect.Array, reflect.Slice:
			i, err := strconv.Atoi(s[0])
			if err != nil {
				panic(fmt.Sprintf("bad array index %s: %s", s[0], err))
			}
			switch v.Type().Elem().Kind() {
			case reflect.Struct:
				prefix = fmt.Sprintf("%s|%d", prefix, i)
			case reflect.Uint8:
				return []string{prefix}
			default:
				return []string{fmt.Sprintf("%s~", prefix)}
			}
			v = v.Index(i)
		case reflect.Struct:
			f, found := v.Type().FieldByName(s[0])
			if !found {
				panic(fmt.Sprintf("could not find field %s", s[0]))
			}
			prefix, _ = calculateKeyFromField(f, prefix, recursion{})
			v = v.FieldByIndex(f.Index)
		default:
			panic(fmt.Sprintf("cannot get field from type %s", v.Type()))
		}

		return calculateKeys(v, field, prefix)
	}

	var out []string
	switch v.Kind() {
	case reflect.Map:
		for _, k := range v.MapKeys() {
			sk := k.Convert(reflect.TypeOf(""))
			prefix := fmt.Sprintf("%s|%s", prefix, sk.String())
			out = append(out, calculateKeys(v.MapIndex(k), field, prefix)...)
		}
	case reflect.Array, reflect.Slice:
		switch v.Type().Elem().Kind() {
		case reflect.Struct:
			for i := 0; i < v.Len(); i++ {
				prefix := fmt.Sprintf("%s|%d", prefix, i)
				out = append(out, calculateKeys(v.Index(i), field, prefix)...)
			}
		case reflect.Uint8:
			return []string{prefix}
		default:
			return []string{fmt.Sprintf("%s~", prefix)}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			prefix, _ := calculateKeyFromField(v.Type().Field(i), prefix, recursion{})
			out = append(out, calculateKeys(v.Field(i), field, prefix)...)
		}
	default:
		panic(fmt.Sprintf("can't iterate type %s", v.Type().String()))
	}

	return out

}

// CalculateKeys gets the keys in extraconfig corresponding to the field
// specification passed in for obj. Examples:
//
//	  type struct A {
//	      I   int    `vic:"0.1" scope:"read-only" key:"i"`
//	      Str string `vic:"0.1" scope:"read-only" key:"str"`
//	  }
//
//	  type struct B {
//	      A A                      `vic:"0.1" scope:"read-only" key:"a"`
//	      Array []A                `vic:"0.1" scope:"read-only" key:"array"`
//	      Map   map[string]string  `vic:"0.1" scope:"read-only" key:"map"`
//	  }
//
//	  b := B{}
//	  b.Array = []A{A{}}
//	  b.Map = map[string]string{"foo": "", "bar": ""}
//	  // returns []string{"a/str"}
//	  CalculateKeys(b, "A.Str", "")
//
//	  // returns []string{"array|0"}
//	  CalculateKeys(b, "Array.0", "")
//
//	  // returns []string{"array|0"}
//	  CalculateKeys(b, "Array.*", "")
//
//	  // returns []string{"map|foo", "map|bar"}
//	  CalculateKeys(b, "Map.*", "")
//
//	  // returns []string{"map|foo"}
//	  CalculateKeys(b, "Map.foo", "")
//
//	  // returns []string{"map|foo/str"}
//	  CalculateKeys(b, "Map.foo.str", "")
//
func CalculateKeys(obj interface{}, field string, prefix string) []string {
	defer log.SetLevel(log.GetLevel())
	log.SetLevel(EncodeLogLevel)

	return calculateKeys(reflect.ValueOf(obj), field, prefix)
}
