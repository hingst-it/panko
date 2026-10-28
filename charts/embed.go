// Package charts embeds the Panko Helm chart, so panko install and panko dev up need no checkout.
// The same files serve helm template, Argo CD and Flux from charts/panko.
package charts

import "embed"

//go:embed all:panko
var FS embed.FS
