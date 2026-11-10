package sourcespec

import (
	"errors"
	"strings"
)

var (
	// ErrEmptyName 表示源名为空（含纯空白）。
	ErrEmptyName = errors.New("empty name")
	// ErrReservedName 表示源名占用了剪枝事件的保留来源名。
	ErrReservedName = errors.New(`"route" 是保留来源名（剪枝记录专用）`)
)

// ValidateName 复用源创建的名字规则；仅判 trim 后的空名和字面 route。
// trim 只参与判定，不改写调用方提供的名字，也不套用能力名的规则。
func ValidateName(name string) error {
	switch strings.TrimSpace(name) {
	case "":
		return ErrEmptyName
	case "route":
		return ErrReservedName
	default:
		return nil
	}
}
