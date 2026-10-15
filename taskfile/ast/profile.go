package ast

import (
	"iter"
	"sync"

	"github.com/elliotchance/orderedmap/v3"
	"go.yaml.in/yaml/v3"

	"github.com/go-task/task/v3/errors"
)

type (
	// Profile is a named execution configuration that can be selected from the
	// command line. It provides its own set of variables, environment
	// variables and dotenv files, and it can extend other profiles.
	Profile struct {
		Desc    string
		Extends []string
		Dotenv  []string
		Vars    *Vars
		Env     *Vars
	}

	// Profiles is an ordered map of profile names to Profiles.
	Profiles struct {
		om    *orderedmap.OrderedMap[string, *Profile]
		mutex sync.RWMutex
	}

	// A ProfileElement is a key-value pair that is used for initializing a
	// Profiles structure.
	ProfileElement orderedmap.Element[string, *Profile]
)

// NewProfiles creates a new instance of Profiles and initializes it with the
// provided set of elements, if any.
func NewProfiles(els ...*ProfileElement) *Profiles {
	profiles := &Profiles{
		om: orderedmap.NewOrderedMap[string, *Profile](),
	}
	for _, el := range els {
		profiles.Set(el.Key, el.Value)
	}
	return profiles
}

// Len returns the number of profiles in the Profiles map.
func (profiles *Profiles) Len() int {
	if profiles == nil || profiles.om == nil {
		return 0
	}
	defer profiles.mutex.RUnlock()
	profiles.mutex.RLock()
	return profiles.om.Len()
}

// Get returns the profile with the provided key and a boolean that indicates
// if the value was found or not.
func (profiles *Profiles) Get(key string) (*Profile, bool) {
	if profiles == nil || profiles.om == nil {
		return nil, false
	}
	defer profiles.mutex.RUnlock()
	profiles.mutex.RLock()
	return profiles.om.Get(key)
}

// Set sets the value of the profile with the provided key to the provided
// value.
func (profiles *Profiles) Set(key string, value *Profile) bool {
	if profiles == nil {
		profiles = NewProfiles()
	}
	if profiles.om == nil {
		profiles.om = orderedmap.NewOrderedMap[string, *Profile]()
	}
	defer profiles.mutex.Unlock()
	profiles.mutex.Lock()
	return profiles.om.Set(key, value)
}

// All returns an iterator that loops over all profile key-value pairs.
func (profiles *Profiles) All() iter.Seq2[string, *Profile] {
	if profiles == nil || profiles.om == nil {
		return func(yield func(string, *Profile) bool) {}
	}
	return profiles.om.AllFromFront()
}

// Keys returns an iterator that loops over all profile keys.
func (profiles *Profiles) Keys() iter.Seq[string] {
	if profiles == nil || profiles.om == nil {
		return func(yield func(string) bool) {}
	}
	return profiles.om.Keys()
}

// Values returns an iterator that loops over all profile values.
func (profiles *Profiles) Values() iter.Seq[*Profile] {
	if profiles == nil || profiles.om == nil {
		return func(yield func(*Profile) bool) {}
	}
	return profiles.om.Values()
}

func (p *Profile) UnmarshalYAML(node *yaml.Node) error {
	if p == nil {
		return errors.NewTaskfileDecodeError(nil, node).WithTypeMessage("profile")
	}

	var profile struct {
		Desc    string
		Extends stringList
		Dotenv  []string
		Vars    *Vars
		Env     *Vars
	}
	if err := node.Decode(&profile); err != nil {
		return errors.NewTaskfileDecodeError(err, node)
	}

	p.Desc = profile.Desc
	p.Extends = profile.Extends
	p.Dotenv = profile.Dotenv
	p.Vars = profile.Vars
	p.Env = profile.Env

	if p.Vars == nil {
		p.Vars = NewVars()
	}
	if p.Env == nil {
		p.Env = NewVars()
	}
	return nil
}

func (profiles *Profiles) UnmarshalYAML(node *yaml.Node) error {
	if profiles == nil {
		return errors.NewTaskfileDecodeError(nil, node).WithTypeMessage("profiles")
	}
	profiles.om = orderedmap.NewOrderedMap[string, *Profile]()
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]

			var p Profile
			if err := valueNode.Decode(&p); err != nil {
				return err
			}

			profiles.Set(keyNode.Value, &p)
		}
		return nil
	}

	return errors.NewTaskfileDecodeError(nil, node).WithTypeMessage("profiles")
}

// stringList accepts either a single string or a list of strings when
// decoding YAML.
type stringList []string

func (s *stringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var v string
		if err := node.Decode(&v); err != nil {
			return errors.NewTaskfileDecodeError(err, node)
		}
		*s = []string{v}
		return nil
	case yaml.SequenceNode:
		var v []string
		if err := node.Decode(&v); err != nil {
			return errors.NewTaskfileDecodeError(err, node)
		}
		*s = v
		return nil
	default:
		return errors.NewTaskfileDecodeError(nil, node).WithMessage(`must be a string or a list of strings`)
	}
}
