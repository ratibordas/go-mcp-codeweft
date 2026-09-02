// Package model contains shared fixture types.

package model

type Creator interface {
	Create(string) Customer
}

type GeneratedCreator interface {
	GeneratedCreate() Customer
}

type RestrictedCreator interface {
	Creator
	~string
}

type Metadata struct {
	Region string
}

type Customer struct {
	Name string
}

func NewCustomer(name string) Customer {
	return Customer{Name: name}
}
