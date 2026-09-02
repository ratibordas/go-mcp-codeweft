package customer

import (
	"strings"

	"example.com/shared/model"
)

type Service struct {
	model.Metadata
}

type HiddenService struct{}

type PointerService struct{}

func (Service) Create(name string) model.Customer {
	return model.NewCustomer(name)
}

func (*PointerService) Create(name string) model.Customer {
	return model.NewCustomer(name)
}

func CreateWith(service Service, name string) model.Customer {
	return service.Create(name)
}

func Clean(name string) string {
	return strings.TrimSpace(name)
}

func InvokeClosure() {
	closure := func() {}
	closure()
}

func init() {
	initOne()
}

func init() {
	initTwo()
}

func initOne() {}

func initTwo() {}
