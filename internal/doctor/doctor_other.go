//go:build !linux

package doctor

func DefaultEnv() Env {
	return fillEnvDefaults(Env{})
}
