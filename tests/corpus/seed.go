// Package publiccorpus is the public synthetic corpus (spam/ and
// legit/<class>/). Its legit samples seed every new private corpus, so a
// private corpus is never tested against zero legit samples.
package publiccorpus

import "embed"

// Legit holds legit/<class>/*.yaml.
//
//go:embed legit
var Legit embed.FS
