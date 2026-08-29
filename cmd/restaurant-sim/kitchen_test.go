package main

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dedupCache — то, чем заведение защищается от повторной доставки события.
// Платформа гарантирует at-least-once, поэтому ошибка здесь означает, что один
// заказ дважды уйдёт на кухню.

func TestDedupCache_FirstSeenThenRepeat(t *testing.T) {
	t.Parallel()

	c := newDedupCache(10)

	assert.True(t, c.Add(1), "первое появление события")
	assert.False(t, c.Add(1), "повтор распознан")
	assert.True(t, c.Add(2), "другое событие проходит")
	assert.False(t, c.Add(2))
}

// Кэш ограничен по размеру: без вытеснения сим копил бы идентификаторы вечно.
func TestDedupCache_EvictsOldest(t *testing.T) {
	t.Parallel()

	const capacity = 3
	c := newDedupCache(capacity)

	for id := int64(1); id <= capacity; id++ {
		require.True(t, c.Add(id))
	}

	// Четвёртое событие вытесняет первое.
	require.True(t, c.Add(4))
	assert.True(t, c.Add(1), "событие 1 вытеснено и снова считается новым")

	// А недавние по-прежнему распознаются как повторы.
	assert.False(t, c.Add(4))
}

// Размер набора не должен расти сверх ёмкости, сколько событий ни пришло.
func TestDedupCache_SizeStaysBounded(t *testing.T) {
	t.Parallel()

	const capacity = 16
	c := newDedupCache(capacity)

	for id := int64(1); id <= 1000; id++ {
		c.Add(id)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	assert.LessOrEqual(t, len(c.seen), capacity)
	assert.LessOrEqual(t, len(c.order), capacity)
	assert.Equal(t, len(c.seen), len(c.order), "карта и очередь вытеснения не разошлись")
}

// Вебхуки приходят параллельно: платформа может доставлять пачку в несколько
// соединений. Ровно один вызов на идентификатор обязан вернуть true.
func TestDedupCache_ConcurrentAddIsExclusive(t *testing.T) {
	t.Parallel()

	c := newDedupCache(1000)

	const racers = 50
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		firsts int
	)

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			if c.Add(42) {
				mu.Lock()
				firsts++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, 1, firsts, "событие принято к обработке ровно один раз")
}

// Конвейер обязан идти по разрешённым переходам конечного автомата платформы:
// NEW → ACCEPTED → COOKING → READY. Любой другой порядок дал бы STATE_CONFLICT
// на каждом заказе.
func TestPipelineFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  simConfig
		want []string
	}{
		{
			name: "автоприёмка ведёт заказ до готовности",
			cfg:  simConfig{AutoAccept: true},
			want: []string{statusAccepted, statusCooking, statusReady},
		},
		{
			name: "аварийный режим отказывает сразу",
			cfg:  simConfig{RejectAll: true},
			want: []string{statusRejected},
		},
		{
			name: "стоп-лист имеет приоритет над автоприёмкой",
			cfg:  simConfig{RejectAll: true, AutoAccept: true},
			want: []string{statusRejected},
		},
		{
			name: "ручной режим не двигает заказ сам",
			cfg:  simConfig{},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stages := pipelineFor(tt.cfg)

			got := make([]string, 0, len(stages))
			for _, s := range stages {
				got = append(got, s.status)
			}

			if tt.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// Тайминги из конфигурации должны доходить до шагов конвейера.
func TestPipelineFor_UsesConfiguredDelays(t *testing.T) {
	t.Parallel()

	cfg := simConfig{
		AutoAccept:  true,
		AcceptDelay: 1 * time.Second,
		CookingTime: 2 * time.Second,
		ReadyDelay:  3 * time.Second,
	}

	stages := pipelineFor(cfg)

	require.Len(t, stages, 3)
	assert.Equal(t, 1*time.Second, stages[0].delay)
	assert.Equal(t, 2*time.Second, stages[1].delay)
	assert.Equal(t, 3*time.Second, stages[2].delay)
}
