package lib

import (
	"errors"
)

var ErrNotFound = errors.New("not found")
var ErrAlreadyExists = errors.New("already exists")

// ErrInvalidPluginConfig wraps a plugin config refused before the plugin is loaded, so callers
// can tell a config the caller got wrong apart from a failure to load a valid one.
var ErrInvalidPluginConfig = errors.New("invalid plugin config")
