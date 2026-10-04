package substrate

import "errors"

var (
	ErrNotFound  = errors.New("substrate: not found")         // sandbox, build or file unknown
	ErrCapacity  = errors.New("substrate: capacity")          // node refused (sandbox cap or starting limit)
	ErrTooLarge  = errors.New("substrate: too large")         // file exceeds a read limit
	ErrNotDir    = errors.New("substrate: not a dir")         // parent is not a directory
	ErrNotEmpty  = errors.New("substrate: not empty")         // non-recursive remove of a non-empty dir
	ErrExist     = errors.New("substrate: exists")            // wrong type exists at the path
	ErrInvalidOp = errors.New("substrate: invalid operation") // e.g. write under an existing file
)
