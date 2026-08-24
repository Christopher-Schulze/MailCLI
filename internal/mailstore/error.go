package mailstore

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) ErrorCode() string {
	return e.Code
}

func operationError(code string, message string) error {
	return &Error{Code: code, Message: message}
}
