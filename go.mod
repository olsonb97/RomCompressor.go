module github.com/olsonb97/romcompressor.go

go 1.25.0

require (
	github.com/pierrec/lz4/v4 v4.1.27
	golang.org/x/sys v0.45.0
)

retract (
	v0.1.0 // Superseded initial release; cached contents do not match this module path.
	v0.2.0 // Withdrawn pre-release publication.
	v1.0.0 // Published accidentally before the initial 0.x release.
	v1.0.1 // Retraction metadata only; use v0.1.1 or a later unretracted release.
)
