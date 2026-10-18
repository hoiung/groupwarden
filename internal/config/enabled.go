package config

// Enabled is a community or a single group an admin switched on from the
// admin chat (/enable). The bot keeps the list in its store; every load
// merges it with the file's communities.
type Enabled struct {
	ID    string // the community's or the group's "…@g.us" ID
	Group bool   // one group on its own (a set of one), not a community
	Name  string
}

// addEnabled adds each entry the file does not already list, with the global
// settings: a community under its ID, a single group as a set of one named by
// its ID. It returns the IDs the file itself lists (its community keys and
// the groups of its sets).
func (c *Config) addEnabled(list []Enabled) map[string]bool {
	inFile := map[string]bool{}
	for id, cm := range c.Communities {
		inFile[id] = true
		for _, g := range cm.Groups {
			inFile[g] = true
		}
	}
	// c.Communities is never nil here: the schema's default for it is {}.
	for _, e := range list {
		if inFile[e.ID] {
			continue
		}
		cm := Community{Name: e.Name}
		if e.Group {
			cm.Groups = []string{e.ID}
		}
		c.Communities[e.ID] = cm
	}
	return inFile
}

// FromFile reports whether the config file itself lists id (a community, or
// a group of a set) rather than /enable.
func (l *Loaded) FromFile(id string) bool { return l.inFile[id] }

// OwnSettings names the settings the entry gives itself instead of taking the
// global ones ("mode", "disable_rules", "word_lists", "retention"); an /enable
// entry has none.
func (cm Community) OwnSettings() []string {
	var own []string
	if cm.Mode != "" {
		own = append(own, "mode")
	}
	if len(cm.DisableRules) > 0 {
		own = append(own, "disable_rules")
	}
	if len(cm.WordLists) > 0 {
		own = append(own, "word_lists")
	}
	if cm.Retention != nil {
		own = append(own, "retention")
	}
	return own
}

// WithEnabled builds the same file contents again with list as the /enable
// entries (the commands beside `run` see what it moderates).
func (l *Loaded) WithEnabled(list []Enabled) (*Loaded, error) {
	nl, err := parseEnabled(l.Raw, l.Config.Dir, list)
	if err != nil {
		return nil, err
	}
	if err := checkCorpus(nl); err != nil {
		return nil, err
	}
	return nl, nil
}
