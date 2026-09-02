package customer

import "example.com/shared/model"

func (HiddenService) Create(name string) model.Customer {
	return model.NewCustomer(name)
}
