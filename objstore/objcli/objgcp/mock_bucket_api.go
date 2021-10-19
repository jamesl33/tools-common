// Code generated by mockery v2.9.4. DO NOT EDIT.

package objgcp

import (
	context "context"

	storage "cloud.google.com/go/storage"
	mock "github.com/stretchr/testify/mock"
)

// mockBucketAPI is an autogenerated mock type for the bucketAPI type
type mockBucketAPI struct {
	mock.Mock
}

// Object provides a mock function with given fields: key
func (_m *mockBucketAPI) Object(key string) objectAPI {
	ret := _m.Called(key)

	var r0 objectAPI
	if rf, ok := ret.Get(0).(func(string) objectAPI); ok {
		r0 = rf(key)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(objectAPI)
		}
	}

	return r0
}

// Objects provides a mock function with given fields: ctx, query
func (_m *mockBucketAPI) Objects(ctx context.Context, query *storage.Query) objectIteratorAPI {
	ret := _m.Called(ctx, query)

	var r0 objectIteratorAPI
	if rf, ok := ret.Get(0).(func(context.Context, *storage.Query) objectIteratorAPI); ok {
		r0 = rf(ctx, query)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(objectIteratorAPI)
		}
	}

	return r0
}
