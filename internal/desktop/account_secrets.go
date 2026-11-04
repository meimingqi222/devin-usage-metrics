package desktop

import "github.com/egoist/mygo"

type accountPasswordStore interface {
	Get(scope string) (string, error)
	Set(scope, password string) error
	Delete(scope string) error
}

type systemAccountPasswords struct{}

func (systemAccountPasswords) Get(scope string) (string, error) {
	value, err := mygo.Secrets.Service("com.devin.usage-metrics.account-sync").Get(scope)
	defer clear(value)
	return string(value), err
}
func (systemAccountPasswords) Set(scope, password string) error {
	value := []byte(password)
	defer clear(value)
	return mygo.Secrets.Service("com.devin.usage-metrics.account-sync").Set(scope, value)
}
func (systemAccountPasswords) Delete(scope string) error {
	return mygo.Secrets.Service("com.devin.usage-metrics.account-sync").Delete(scope)
}
