package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"billgo/internal/transport"
)

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func TestClassifySend(t *testing.T) {
	ctx := context.Background()
	if ce := asCall(classifySend(ctx, fmt.Errorf("%w: %w", transport.ErrTransport, fakeTimeout{}))); ce.code != codeTimeout {
		t.Fatalf("超时应单独分类，得到 %+v", ce)
	}
	if ce := asCall(classifySend(ctx, fmt.Errorf("%w: dial tcp: connection refused", transport.ErrTransport))); ce.code != codeUnreachable {
		t.Fatalf("连不上应是不可达，得到 %+v", ce)
	}
	var ne net.Error = fakeTimeout{}
	if !ne.Timeout() {
		t.Fatal("测试错误应实现 net.Error")
	}
	se := &transport.StatusError{Code: 500, Body: "boom"}
	if ce := asCall(classifySend(ctx, se)); ce.code != codeRemote {
		t.Fatalf("HTTP 错误应是 remote_error，得到 %+v", ce)
	}
	tctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if ce := asCall(classifySend(tctx, context.DeadlineExceeded)); ce.code != codeTimeout {
		t.Fatalf("上下文超时应是 timeout，得到 %+v", ce)
	}
}

func asCall(err error) *callError {
	var ce *callError
	if !errors.As(err, &ce) {
		return &callError{code: "missing", msg: err.Error()}
	}
	return ce
}
