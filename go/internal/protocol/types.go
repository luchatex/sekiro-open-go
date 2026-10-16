package protocol

// Type 包类型字节，与 SekiroPacketType 枚举 code 对齐。
type Type byte

const (
	TypeHeartbeat          Type = 0  // TYPE_HEARTBEAT
	CTypeRegister          Type = 16 // C_TYPE_REGISTER
	CTypeInvokeResponse    Type = 17 // C_TYPE_INVOKE_RESPONSE
	CTypeOffline           Type = 18 // C_TYPE_OFFLINE
	CTypeCmdResponse       Type = 19 // C_TYPE_CMD_RESPONSE
	CTypeInvokeStreamClose Type = 20 // C_TYPE_INVOKE_STREAM_CLOSE
	STypeInvoke            Type = 32 // S_TYPE_INVOKE
	STypeCmd               Type = 33 // S_TYPE_CMD
	STypeOfflineResponse   Type = 34 // S_TYPE_OFFLINE_RESPONSE
	STypeInvokeResponse    Type = 35 // S_TYPE_INVOKE_RESPONSE
	STypeInvokeConnected   Type = 36 // S_TYPE_INVOKE_CONNECTED
	ITypeConnect           Type = 48 // I_TYPE_CONNECT
	ITypeInvoke            Type = 49 // I_TYPE_INVOKE
)

var typeNames = map[Type]string{
	TypeHeartbeat:          "TYPE_HEARTBEAT",
	CTypeRegister:          "C_TYPE_REGISTER",
	CTypeInvokeResponse:    "C_TYPE_INVOKE_RESPONSE",
	CTypeOffline:           "C_TYPE_OFFLINE",
	CTypeCmdResponse:       "C_TYPE_CMD_RESPONSE",
	CTypeInvokeStreamClose: "C_TYPE_INVOKE_STREAM_CLOSE",
	STypeInvoke:            "S_TYPE_INVOKE",
	STypeCmd:               "S_TYPE_CMD",
	STypeOfflineResponse:   "S_TYPE_OFFLINE_RESPONSE",
	STypeInvokeResponse:    "S_TYPE_INVOKE_RESPONSE",
	STypeInvokeConnected:   "S_TYPE_INVOKE_CONNECTED",
	ITypeConnect:           "I_TYPE_CONNECT",
	ITypeInvoke:            "I_TYPE_INVOKE",
}

func (t Type) String() string {
	if s, ok := typeNames[t]; ok {
		return s
	}
	return "UNKNOWN"
}

// IsInvokeResponse 是否为响应类包（压缩层仅对该类包生效）。
func (t Type) IsInvokeResponse() bool {
	return t == CTypeInvokeResponse || t == STypeInvokeResponse
}
