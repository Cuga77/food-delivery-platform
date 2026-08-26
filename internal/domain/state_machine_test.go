package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/domain"
)

func allStatuses() []domain.OrderStatus {
	return []domain.OrderStatus{
		domain.StatusNew, domain.StatusAccepted, domain.StatusRejected,
		domain.StatusCooking, domain.StatusReady, domain.StatusDelivered,
		domain.StatusCancelled,
	}
}

func allActors() []domain.Actor {
	return []domain.Actor{domain.ActorUser, domain.ActorRestaurant, domain.ActorSystem}
}

// allowedTriples — исчерпывающий список разрешённых переходов. Любой переход,
// которого здесь нет, обязан быть запрещён: это проверяется ниже перебором всей
// матрицы (7 статусов × 7 статусов × 3 актора).
func allowedTriples() []struct {
	from  domain.OrderStatus
	to    domain.OrderStatus
	actor domain.Actor
} {
	return []struct {
		from  domain.OrderStatus
		to    domain.OrderStatus
		actor domain.Actor
	}{
		{domain.StatusNew, domain.StatusAccepted, domain.ActorRestaurant},
		{domain.StatusNew, domain.StatusRejected, domain.ActorRestaurant},
		{domain.StatusNew, domain.StatusCancelled, domain.ActorUser},
		{domain.StatusNew, domain.StatusCancelled, domain.ActorSystem},
		{domain.StatusAccepted, domain.StatusCooking, domain.ActorRestaurant},
		{domain.StatusAccepted, domain.StatusCancelled, domain.ActorRestaurant},
		{domain.StatusCooking, domain.StatusReady, domain.ActorRestaurant},
		{domain.StatusCooking, domain.StatusCancelled, domain.ActorRestaurant},
		{domain.StatusReady, domain.StatusDelivered, domain.ActorRestaurant},
	}
}

func TestCanTransition_AllowedSet(t *testing.T) {
	t.Parallel()

	for _, tc := range allowedTriples() {
		t.Run(string(tc.from)+"→"+string(tc.to)+"/"+string(tc.actor), func(t *testing.T) {
			t.Parallel()
			assert.True(t, domain.CanTransition(tc.from, tc.to, tc.actor))
			assert.NoError(t, domain.ValidateTransition(tc.from, tc.to, tc.actor))
		})
	}
}

// TestCanTransition_FullMatrix перебирает все комбинации и требует, чтобы
// разрешёнными были ровно те, что перечислены в allowedTriples. Это защищает от
// случайного расширения графа при рефакторинге.
func TestCanTransition_FullMatrix(t *testing.T) {
	t.Parallel()

	type triple struct {
		from  domain.OrderStatus
		to    domain.OrderStatus
		actor domain.Actor
	}

	allowed := make(map[triple]struct{})
	for _, tc := range allowedTriples() {
		allowed[triple{tc.from, tc.to, tc.actor}] = struct{}{}
	}

	for _, from := range allStatuses() {
		for _, to := range allStatuses() {
			for _, actor := range allActors() {
				_, want := allowed[triple{from, to, actor}]
				got := domain.CanTransition(from, to, actor)
				assert.Equalf(t, want, got,
					"переход %s → %s для %s: ожидалось allowed=%v", from, to, actor, want)
			}
		}
	}
}

func TestValidateTransition_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		from     domain.OrderStatus
		to       domain.OrderStatus
		actor    domain.Actor
		wantCode domain.ErrorCode
	}{
		{
			name:     "пользователь не отменяет принятый заказ",
			from:     domain.StatusAccepted,
			to:       domain.StatusCancelled,
			actor:    domain.ActorUser,
			wantCode: domain.CodeOrderCannotBeCancelled,
		},
		{
			name:     "пользователь не отменяет готовый заказ",
			from:     domain.StatusReady,
			to:       domain.StatusCancelled,
			actor:    domain.ActorUser,
			wantCode: domain.CodeOrderCannotBeCancelled,
		},
		{
			name:     "из терминального статуса переходов нет",
			from:     domain.StatusDelivered,
			to:       domain.StatusCooking,
			actor:    domain.ActorRestaurant,
			wantCode: domain.CodeStateConflict,
		},
		{
			name:     "нельзя перепрыгнуть через готовку",
			from:     domain.StatusAccepted,
			to:       domain.StatusReady,
			actor:    domain.ActorRestaurant,
			wantCode: domain.CodeStateConflict,
		},
		{
			name:     "пользователь не двигает заказ по конвейеру",
			from:     domain.StatusNew,
			to:       domain.StatusAccepted,
			actor:    domain.ActorUser,
			wantCode: domain.CodeStateConflict,
		},
		{
			name:     "system не принимает заказ за заведение",
			from:     domain.StatusNew,
			to:       domain.StatusAccepted,
			actor:    domain.ActorSystem,
			wantCode: domain.CodeStateConflict,
		},
		{
			name:     "неизвестный статус отсекается валидацией",
			from:     domain.StatusNew,
			to:       domain.OrderStatus("FLYING"),
			actor:    domain.ActorRestaurant,
			wantCode: domain.CodeValidationError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := domain.ValidateTransition(tt.from, tt.to, tt.actor)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, domain.CodeOf(err))
		})
	}
}

func TestOrderStatus_IsTerminal(t *testing.T) {
	t.Parallel()

	terminal := map[domain.OrderStatus]bool{
		domain.StatusRejected:  true,
		domain.StatusDelivered: true,
		domain.StatusCancelled: true,
	}

	for _, s := range allStatuses() {
		assert.Equalf(t, terminal[s], s.IsTerminal(), "статус %s", s)
	}
}

func TestOrderStatus_ReleasesStock(t *testing.T) {
	t.Parallel()

	releases := map[domain.OrderStatus]bool{
		domain.StatusRejected:  true,
		domain.StatusCancelled: true,
	}

	for _, s := range allStatuses() {
		assert.Equalf(t, releases[s], s.ReleasesStock(), "статус %s", s)
	}
}

func TestAllowedNextStatuses_IsSorted(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		[]domain.OrderStatus{domain.StatusAccepted, domain.StatusCancelled, domain.StatusRejected},
		domain.AllowedNextStatuses(domain.StatusNew))
	assert.Empty(t, domain.AllowedNextStatuses(domain.StatusDelivered))
}

// TestEveryNonTerminalStatusReachesTerminal гарантирует отсутствие «ловушек»:
// из любого нетерминального статуса существует путь в терминальный.
func TestEveryNonTerminalStatusReachesTerminal(t *testing.T) {
	t.Parallel()

	for _, start := range allStatuses() {
		if start.IsTerminal() {
			continue
		}

		visited := map[domain.OrderStatus]bool{start: true}
		queue := []domain.OrderStatus{start}
		reached := false

		for len(queue) > 0 && !reached {
			cur := queue[0]
			queue = queue[1:]
			for _, next := range domain.AllowedNextStatuses(cur) {
				if next.IsTerminal() {
					reached = true
					break
				}
				if !visited[next] {
					visited[next] = true
					queue = append(queue, next)
				}
			}
		}

		assert.Truef(t, reached, "из статуса %s недостижим ни один терминальный", start)
	}
}
