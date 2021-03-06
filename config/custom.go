package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	envoy_config_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/mitchellh/mapstructure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v2"
)

// A StringSlice is a slice of strings.
type StringSlice []string

// NewStringSlice creatse a new StringSlice.
func NewStringSlice(values ...string) StringSlice {
	return StringSlice(values)
}

const (
	array = iota
	arrayValue
	object
	objectKey
	objectValue
)

// UnmarshalJSON unmarshals a JSON document into the string slice.
func (slc *StringSlice) UnmarshalJSON(data []byte) error {
	typeStack := []int{array}
	stateStack := []int{arrayValue}

	var vals []string
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := dec.Token()
		if err != nil {
			break
		}

		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '[':
				typeStack = append(typeStack, array)
				stateStack = append(stateStack, arrayValue)
			case '{':
				typeStack = append(typeStack, object)
				stateStack = append(stateStack, objectKey)
			case ']', '}':
				typeStack = typeStack[:len(typeStack)-1]
				stateStack = stateStack[:len(stateStack)-1]
			}
			continue
		}

		switch stateStack[len(stateStack)-1] {
		case objectKey:
			stateStack[len(stateStack)-1] = objectValue
		case objectValue:
			stateStack[len(stateStack)-1] = objectKey
			fallthrough
		default:
			switch t := token.(type) {
			case bool:
				vals = append(vals, fmt.Sprint(t))
			case float64:
				vals = append(vals, fmt.Sprint(t))
			case json.Number:
				vals = append(vals, fmt.Sprint(t))
			case string:
				vals = append(vals, t)
			default:
			}
		}
	}
	*slc = StringSlice(vals)
	return nil
}

// UnmarshalYAML unmarshals a YAML document into the string slice. UnmarshalJSON is
// reused as the actual implementation.
func (slc *StringSlice) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var i interface{}
	err := unmarshal(&i)
	if err != nil {
		return err
	}
	bs, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return slc.UnmarshalJSON(bs)
}

// WeightedURL is a way to specify an upstream with load balancing weight attached to it
type WeightedURL struct {
	URL url.URL
	// LbWeight is a relative load balancer weight for this upstream URL
	// zero means not assigned
	LbWeight uint32
}

func (u *WeightedURL) Validate() error {
	if u.URL.Hostname() == "" {
		return errHostnameMustBeSpecified
	}
	if u.URL.Scheme == "" {
		return errSchemeMustBeSpecified
	}
	return nil
}

// ParseWeightedURL parses url that has an optional weight appended to it
func ParseWeightedURL(dst string) (*WeightedURL, error) {
	to, w, err := weightedString(dst)
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(to)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", to, err)
	}

	if u.Hostname() == "" {
		return nil, errHostnameMustBeSpecified
	}

	return &WeightedURL{*u, w}, nil
}

func (u *WeightedURL) String() string {
	str := u.URL.String()
	if u.LbWeight == 0 {
		return str
	}
	return fmt.Sprintf("{url=%s, weight=%d}", str, u.LbWeight)
}

type WeightedURLs []WeightedURL

// ParseWeightedUrls parses
func ParseWeightedUrls(urls ...string) (WeightedURLs, error) {
	out := make([]WeightedURL, 0, len(urls))

	for _, dst := range urls {
		u, err := ParseWeightedURL(dst)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}

	if _, err := WeightedURLs(out).Validate(); err != nil {
		return nil, err
	}

	return out, nil
}

// HasWeight indicates if url group has weights assigned
type HasWeight bool

// Validate checks that URLs are valid, and either all or none have weights assigned
func (urls WeightedURLs) Validate() (HasWeight, error) {
	if len(urls) == 0 {
		return false, errEmptyUrls
	}

	noWeight := false
	hasWeight := false

	for i := range urls {
		if err := urls[i].Validate(); err != nil {
			return false, fmt.Errorf("%s: %w", urls[i].String(), err)
		}
		if urls[i].LbWeight == 0 {
			noWeight = true
		} else {
			hasWeight = true
		}
	}

	if noWeight == hasWeight {
		return false, errEndpointWeightsSpec
	}

	if noWeight {
		return HasWeight(false), nil
	}
	return HasWeight(true), nil
}

// Flatten converts weighted url array into indidual arrays of urls and weights
func (urls WeightedURLs) Flatten() ([]string, []uint32, error) {
	hasWeight, err := urls.Validate()
	if err != nil {
		return nil, nil, err
	}

	str := make([]string, 0, len(urls))
	wghts := make([]uint32, 0, len(urls))

	for i := range urls {
		str = append(str, urls[i].URL.String())
		wghts = append(wghts, urls[i].LbWeight)
	}

	if !hasWeight {
		return str, nil, nil
	}
	return str, wghts, nil
}

func DecodePolicyBase64Hook() mapstructure.DecodeHookFunc {
	return func(f, t reflect.Type, data interface{}) (interface{}, error) {
		if t != reflect.TypeOf([]Policy{}) {
			return data, nil
		}

		str, ok := data.([]string)
		if !ok {
			return data, nil
		}

		if len(str) != 1 {
			return nil, fmt.Errorf("base64 policy data: expecting 1, got %d", len(str))
		}

		bytes, err := base64.StdEncoding.DecodeString(str[0])
		if err != nil {
			return nil, fmt.Errorf("base64 decoding policy data: %w", err)
		}

		out := []map[interface{}]interface{}{}
		if err = yaml.Unmarshal(bytes, &out); err != nil {
			return nil, fmt.Errorf("parsing base64-encoded policy data as yaml: %w", err)
		}

		return out, nil
	}
}

func DecodePolicyHookFunc() mapstructure.DecodeHookFunc {
	return func(f, t reflect.Type, data interface{}) (interface{}, error) {
		if t != reflect.TypeOf(Policy{}) {
			return data, nil
		}

		// convert all keys to strings so that it can be serialized back to JSON
		// and read by jsonproto package into Envoy's cluster structure
		mp, err := serializable(data)
		if err != nil {
			return nil, err
		}
		ms, ok := mp.(map[string]interface{})
		if !ok {
			return nil, errKeysMustBeStrings
		}

		return parsePolicy(ms)
	}
}

func parsePolicy(src map[string]interface{}) (out map[string]interface{}, err error) {
	out = make(map[string]interface{}, len(src))
	for k, v := range src {
		if k == toKey {
			if v, err = parseTo(v); err != nil {
				return nil, err
			}
		}
		out[k] = v
	}

	// also, interpret the entire policy as Envoy's Cluster document to derive its options
	out[envoyOptsKey], err = parseEnvoyClusterOpts(src)
	if err != nil {
		return nil, err
	}

	return out, nil
}

func parseTo(raw interface{}) ([]WeightedURL, error) {
	rawBS, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var slc StringSlice
	err = json.Unmarshal(rawBS, &slc)
	if err != nil {
		return nil, err
	}

	return ParseWeightedUrls(slc...)
}

func weightedStrings(src StringSlice) (endpoints StringSlice, weights []uint32, err error) {
	weights = make([]uint32, len(src))
	endpoints = make([]string, len(src))

	noWeight := false
	hasWeight := false
	for i, str := range src {
		endpoints[i], weights[i], err = weightedString(str)
		if err != nil {
			return nil, nil, err
		}
		if weights[i] == 0 {
			noWeight = true
		} else {
			hasWeight = true
		}
	}

	if noWeight == hasWeight {
		return nil, nil, errEndpointWeightsSpec
	}

	if noWeight {
		return endpoints, nil, nil
	}
	return endpoints, weights, nil
}

// parses URL followed by weighted
func weightedString(str string) (string, uint32, error) {
	i := strings.IndexRune(str, ',')
	if i < 0 {
		return str, 0, nil
	}

	w, err := strconv.ParseUint(str[i+1:], 10, 32)
	if err != nil {
		return "", 0, err
	}

	if w == 0 {
		return "", 0, errZeroWeight
	}

	return str[:i], uint32(w), nil
}

// parseEnvoyClusterOpts parses src as envoy cluster spec https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/cluster/v3/cluster.proto
// on top of some pre-filled default values
func parseEnvoyClusterOpts(src map[string]interface{}) (*envoy_config_cluster_v3.Cluster, error) {
	c := new(envoy_config_cluster_v3.Cluster)
	if err := parseJSONPB(src, c, protoPartial); err != nil {
		return nil, err
	}

	return c, nil
}

// parseJSONPB takes an intermediate representation and parses it using protobuf parser
// that correctly handles oneof and other data types
func parseJSONPB(src map[string]interface{}, dst proto.Message, opts protojson.UnmarshalOptions) error {
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}

	return opts.Unmarshal(data, dst)
}

// serializable converts mapstructure nested map into map[string]interface{} that is serializable to JSON
func serializable(in interface{}) (interface{}, error) {
	switch typed := in.(type) {
	case map[interface{}]interface{}:
		m := make(map[string]interface{})
		for k, v := range typed {
			kstr, ok := k.(string)
			if !ok {
				return nil, errKeysMustBeStrings
			}
			val, err := serializable(v)
			if err != nil {
				return nil, err
			}
			m[kstr] = val
		}
		return m, nil
	case []interface{}:
		out := make([]interface{}, 0, len(typed))
		for _, elem := range typed {
			val, err := serializable(elem)
			if err != nil {
				return nil, err
			}
			out = append(out, val)
		}
		return out, nil
	default:
		return in, nil
	}
}
