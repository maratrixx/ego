package egrpc

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/gotomicro/ego/core/eapp"
	"github.com/gotomicro/ego/core/ecode"
	"github.com/gotomicro/ego/core/elog"
	"github.com/gotomicro/ego/core/emetric"
	"github.com/gotomicro/ego/core/etrace"
	"github.com/gotomicro/ego/core/util/xcpu"
	"github.com/gotomicro/ego/core/util/xstring"
)

func prometheusUnaryServerInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	startTime := time.Now()
	resp, err := handler(ctx, req)
	code := ecode.ExtractCodes(err)
	// 收敛err错误，将err过滤后，可以知道err是否为系统错误码
	// 只记录系统级别错误
	if code.Code < ecode.EcodeNum {
		// 只记录系统级别的详细错误码
		emetric.ServerHandleHistogram.Observe(time.Since(startTime).Seconds(), emetric.TypeGRPCUnary, info.FullMethod, extractApp(ctx))
		emetric.ServerHandleCounter.Inc(emetric.TypeGRPCUnary, info.FullMethod, extractApp(ctx), code.GetMessage())
	} else {
		emetric.ServerHandleHistogram.Observe(time.Since(startTime).Seconds(), emetric.TypeGRPCUnary, info.FullMethod, extractApp(ctx))
		emetric.ServerHandleCounter.Inc(emetric.TypeGRPCUnary, info.FullMethod, extractApp(ctx), "biz error")
	}

	return resp, err
}

func prometheusStreamServerInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	startTime := time.Now()
	err := handler(srv, ss)
	code := ecode.ExtractCodes(err)
	emetric.ServerHandleHistogram.Observe(time.Since(startTime).Seconds(), emetric.TypeGRPCStream, info.FullMethod, extractApp(ss.Context()))
	emetric.ServerHandleCounter.Inc(emetric.TypeGRPCStream, info.FullMethod, extractApp(ss.Context()), code.GetMessage())
	return err
}

func traceUnaryServerInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	span, ctx := etrace.StartSpanFromContext(
		ctx,
		info.FullMethod,
		etrace.FromIncomingContext(ctx),
		etrace.TagComponent("gRPC"),
		etrace.TagSpanKind("server.unary"),
	)

	defer span.Finish()
	resp, err := handler(ctx, req)

	if err != nil {
		code := codes.Unknown
		if s, ok := status.FromError(err); ok {
			code = s.Code()
		}
		span.SetTag("code", code)
		ext.Error.Set(span, true)
		span.LogFields(etrace.String("event", "error"), etrace.String("message", err.Error()))
	}
	return resp, err
}

type contextedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context ...
func (css contextedServerStream) Context() context.Context {
	return css.ctx
}

func traceStreamServerInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	span, ctx := etrace.StartSpanFromContext(
		ss.Context(),
		info.FullMethod,
		etrace.FromIncomingContext(ss.Context()),
		etrace.TagComponent("gRPC"),
		etrace.TagSpanKind("server.stream"),
		etrace.CustomTag("isServerStream", info.IsServerStream),
	)
	defer span.Finish()

	return handler(srv, contextedServerStream{
		ServerStream: ss,
		ctx:          ctx,
	})
}

func extractApp(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		return strings.Join(md.Get("app"), ",")
	}
	return "unknown"
}

func defaultStreamServerInterceptor(logger *elog.Component, config *Config) grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		var beg = time.Now()
		var fields = make([]elog.Field, 0, 8)
		var event = "normal"
		defer func() {
			cost := time.Since(beg)

			if config.SlowLogThreshold > time.Duration(0) && config.SlowLogThreshold < cost {
				event = "slow"
			}

			if rec := recover(); rec != nil {
				switch rec := rec.(type) {
				case error:
					err = rec
				default:
					err = fmt.Errorf("%v", rec)
				}
				stack := make([]byte, 4096)
				stack = stack[:runtime.Stack(stack, true)]
				fields = append(fields, elog.FieldStack(stack))
				event = "recover"
			}

			fields = append(fields,
				elog.FieldType("stream"),
				elog.FieldCode(ecode.ExtractCodes(err).Code),
				elog.FieldMethod(info.FullMethod),
				elog.FieldCost(time.Since(beg)),
				elog.FieldPeerName(getPeerName(stream.Context())),
				elog.FieldPeerIP(getPeerIP(stream.Context())),
			)

			if err != nil {
				fields = append(fields, elog.FieldErr(err))
				logger.Error("access", fields...)
				return
			}
			if event == "slow" {
				logger.Warn("access", fields...)
			} else {
				logger.Info("access", fields...)
			}
		}()
		return handler(srv, stream)
	}
}

func defaultUnaryServerInterceptor(logger *elog.Component, config *Config) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (res interface{}, err error) {
		var beg = time.Now()
		// 为了性能考虑，如果要加日志字段，需要改变slice大小
		var fields = make([]elog.Field, 0, 20)
		var event = "normal"

		// 此处必须使用defer来recover handler内部可能出现的panic
		defer func() {
			cost := time.Since(beg)
			if rec := recover(); rec != nil {
				switch rec := rec.(type) {
				case error:
					err = rec
				default:
					err = fmt.Errorf("%v", rec)
				}

				stack := make([]byte, 4096)
				stack = stack[:runtime.Stack(stack, true)]
				fields = append(fields, elog.FieldStack(stack))
				event = "recover"
			}

			fields = append(fields,
				elog.FieldType("unary"),
				elog.FieldCode(ecode.ExtractCodes(err).Code),
				elog.FieldEvent(event),
				elog.FieldMethod(info.FullMethod),
				elog.FieldCost(time.Since(beg)),
				elog.FieldPeerName(getPeerName(ctx)),
				elog.FieldPeerIP(getPeerIP(ctx)),
			)

			if config.EnableTraceInterceptor && opentracing.IsGlobalTracerRegistered() {
				fields = append(fields, elog.FieldTid(etrace.ExtractTraceID(ctx)))
			}

			if config.EnableAccessInterceptorReq {
				var req = map[string]interface{}{
					"payload": xstring.JSON(req),
				}
				if md, ok := metadata.FromIncomingContext(ctx); ok {
					req["metadata"] = md
				}
				fields = append(fields, elog.Any("req", req))
			}
			if config.EnableAccessInterceptorRes {
				fields = append(fields, elog.Any("res", map[string]interface{}{
					"payload": xstring.JSON(res),
				}))
			}

			for _, key := range eapp.EgoLogExtraKeys() {
				if value := getContextValue(key, ctx); value != "" {
					fields = append(fields, elog.FieldCustomKeyValue(key, value))
				}
			}

			if config.SlowLogThreshold > time.Duration(0) && config.SlowLogThreshold < cost {
				logger.Warn("slow", fields...)
			}

			if err != nil {
				fields = append(fields, elog.FieldErr(err))
				logger.Error("access", fields...)
				return
			}
			logger.Info("access", fields...)
		}()

		if enableCPUUsage(ctx) {
			var stat = xcpu.Stat{}
			xcpu.ReadStat(&stat)
			if stat.Usage > 0 {
				// https://github.com/grpc/grpc-go/blob/master/Documentation/grpc-metadata.md
				header := metadata.Pairs("cpu-usage", strconv.Itoa(int(stat.Usage)))
				err = grpc.SetHeader(ctx, header)
				if err != nil {
					logger.Error("set header error", elog.FieldErr(err))
				}
			}
		}
		return handler(ctx, req)
	}
}

// enableCPUUsage 是否开启cpu利用率
func enableCPUUsage(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	val, ok2 := md["enable-cpu-usage"]
	if !ok2 {
		return false
	}
	return strings.Join(val, ";") == "true"
}

// getPeerName 获取对端应用名称
func getPeerName(ctx context.Context) string {
	return getContextValue("app", ctx)
}

// getPeerIP 获取对端ip
func getPeerIP(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	// 从metadata里取对端ip
	if val, ok := md["client-ip"]; ok {
		return strings.Join(val, ";")
	}
	// 从grpc里取对端ip
	pr, ok2 := peer.FromContext(ctx)
	if !ok2 {
		return ""
	}
	if pr.Addr == net.Addr(nil) {
		return ""
	}
	addSlice := strings.Split(pr.Addr.String(), ":")
	if len(addSlice) > 1 {
		return addSlice[0]
	}
	return ""
}

func getContextValue(key string, ctx context.Context) string {
	if key == "" {
		return ""
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	val, ok2 := md[key]
	if !ok2 {
		return ""
	}
	return strings.Join(val, ";")
}
