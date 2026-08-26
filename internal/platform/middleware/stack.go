package middleware

import "runtime"

// stack снимает стек текущей горутины для лога паники.
func stack() []byte {
	const maxStackSize = 16 << 10 // 16 КБ хватает, чтобы дойти до причины
	buf := make([]byte, maxStackSize)
	n := runtime.Stack(buf, false)
	return buf[:n]
}
