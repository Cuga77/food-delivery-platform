package app

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"avito-kitchen/internal/domain"
)

// OrderService — сценарии жизненного цикла заказа: оформление, просмотр и
// переходы по статусам. Все изменяющие сценарии выполняются в одной транзакции
// PostgreSQL, поэтому частично применённого заказа не существует.
type OrderService struct {
	tx          TxManager
	restaurants RestaurantRepo
	menus       MenuRepo
	orders      OrderRepo
	outbox      OutboxRepo
	now         func() time.Time
}

// NewOrderService собирает сервис заказов.
func NewOrderService(
	tx TxManager,
	restaurants RestaurantRepo,
	menus MenuRepo,
	orders OrderRepo,
	outbox OutboxRepo,
) *OrderService {
	return &OrderService{
		tx:          tx,
		restaurants: restaurants,
		menus:       menus,
		orders:      orders,
		outbox:      outbox,
		now:         time.Now,
	}
}

const (
	defaultUserOrdersLimit = 20
	maxUserOrdersLimit     = 100
	// maxUserExternalIDLen совпадает с VARCHAR(128) у orders.user_external_id:
	// более длинный идентификатор заведомо ничего не найдёт, и отвечать на него
	// походом в БД незачем.
	maxUserExternalIDLen = 128
)

// OrderPage — страница списка заказов с курсором на следующую.
type OrderPage struct {
	Items []domain.Order
	// NextCursor пуст, когда данные закончились.
	NextCursor string
}

// paginateOrders превращает выборку «лимит + 1» в страницу.
//
// Лишняя запись наружу не отдаётся: она нужна только чтобы отличить «страница
// заполнилась ровно» от «дальше ещё есть данные». Иначе последняя страница
// всегда возвращала бы курсор, а клиент делал бы лишний пустой запрос.
func paginateOrders(items []domain.Order, limit int32) OrderPage {
	if len(items) <= int(limit) {
		return OrderPage{Items: items}
	}

	page := OrderPage{Items: items[:limit]}
	last := page.Items[len(page.Items)-1]
	page.NextCursor = EncodeOrderCursor(OrderCursor{CreatedAt: last.CreatedAt, ID: last.ID})

	return page
}

// ListUserOrdersQuery — входные параметры истории заказов пользователя.
type ListUserOrdersQuery struct {
	UserExternalID string
	Limit          int32
	Cursor         string
}

// ListByUser отдаёт историю заказов пользователя, свежие первыми.
//
// Аутентификации пользователей в MVP нет, поэтому user_external_id здесь —
// ключ доступа к истории, ровно как public_number к отдельному заказу: знаешь
// значение — видишь данные. Веб-клиент выдаёт каждому браузеру собственный
// случайный идентификатор, поэтому перебрать чужую историю нельзя, но это
// свойство держится на неугадываемости значения, а не на проверке прав.
// Ограничение осознанное и зафиксировано в ADR-0004.
func (s *OrderService) ListByUser(ctx context.Context, q ListUserOrdersQuery) (OrderPage, error) {
	if q.UserExternalID == "" {
		return OrderPage{}, domain.Errorf(domain.CodeValidationError,
			"не указан user_external_id")
	}
	if len(q.UserExternalID) > maxUserExternalIDLen {
		return OrderPage{}, domain.Errorf(domain.CodeValidationError,
			"user_external_id длиннее %d символов", maxUserExternalIDLen)
	}

	limit := normalizeLimit(q.Limit, defaultUserOrdersLimit, maxUserOrdersLimit)

	after, err := DecodeOrderCursor(q.Cursor)
	if err != nil {
		return OrderPage{}, err
	}

	items, err := s.orders.ListByUser(ctx, UserOrderFilter{
		UserExternalID: q.UserExternalID,
		After:          after,
		Limit:          limit + 1,
	})
	if err != nil {
		return OrderPage{}, err
	}

	return paginateOrders(items, limit), nil
}

// reservation — намерение списать остаток конкретной позиции меню.
type reservation struct {
	productID int64
	qty       int32
}

// Create оформляет заказ.
//
// Порядок шагов внутри транзакции соответствует бизнес-правилам:
//  1. заведение существует и принимает заказы;
//  2. позиции есть в опубликованном меню и доступны;
//  3. сумма считается по ценам из БД и проходит проверку минимального заказа;
//  4. остатки списываются атомарно;
//  5. заказ, снапшот позиций и стартовое событие истории вставляются вместе;
//  6. в outbox кладётся событие для заведения.
//
// Любая ошибка на шагах 1–6 откатывает транзакцию целиком, включая уже
// выполненные списания остатков. Проверки, не требующие блокировок, идут
// раньше тех, что их берут: обречённый заказ не должен занимать очередь на
// популярной позиции.
func (s *OrderService) Create(ctx context.Context, draft domain.OrderDraft) (domain.Order, error) {
	if err := draft.Validate(); err != nil {
		return domain.Order{}, err
	}

	publicNumber, err := uuid.NewV7()
	if err != nil {
		return domain.Order{}, domain.WrapErrorf(err, domain.CodeInternalError,
			"не удалось сгенерировать номер заказа")
	}

	var created domain.Order

	txErr := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		restaurant, err := s.restaurants.GetByID(ctx, draft.RestaurantID)
		if err != nil {
			return err
		}
		if err := restaurant.EnsureAcceptsOrders(); err != nil {
			return err
		}

		snapshot, err := s.menus.GetPublished(ctx, restaurant.ID)
		if err != nil {
			return err
		}

		items, reservations, subtotal, err := resolveItems(snapshot, draft.Items)
		if err != nil {
			return err
		}

		// Минимальная сумма проверяется ДО списания остатков.
		//
		// Обе проверки внутри одной транзакции, и откат вернул бы остатки в
		// любом порядке — но списание берёт строчные блокировки на позиции
		// меню, а проверка суммы считается по уже прочитанному снапшоту и не
		// требует ни одной. Списывать первым значит заставить заведомо
		// обречённый заказ подержать блокировки популярных позиций, пока
		// остальные покупатели ждут в очереди. Нагрузочный замер показывает,
		// что очередь на строке — самое дорогое место сервиса (load/README.md),
		// и занимать её впустую нельзя.
		if err := restaurant.EnsureMinOrder(subtotal); err != nil {
			return err
		}

		// Списываем остатки строго по возрастанию product_id. Порядок взятия
		// строчных блокировок одинаков для всех конкурирующих транзакций,
		// поэтому взаимная блокировка (deadlock) на пересекающихся корзинах
		// невозможна.
		sort.Slice(reservations, func(i, j int) bool {
			return reservations[i].productID < reservations[j].productID
		})
		for _, r := range reservations {
			if err := s.menus.DecreaseStock(ctx, r.productID, r.qty); err != nil {
				return err
			}
		}

		order := domain.Order{
			PublicNumber:       publicNumber,
			UserExternalID:     draft.UserExternalID,
			RestaurantID:       restaurant.ID,
			RestaurantSlug:     restaurant.Slug,
			Status:             domain.StatusNew,
			DeliveryAddress:    draft.DeliveryAddress,
			SubtotalKopecks:    subtotal,
			DeliveryFeeKopecks: restaurant.DeliveryFeeKopecks,
			TotalKopecks:       subtotal + restaurant.DeliveryFeeKopecks,
			Version:            1,
			Items:              items,
		}

		if err := s.orders.Create(ctx, &order); err != nil {
			return err
		}

		event, err := newOrderCreatedEvent(order)
		if err != nil {
			return err
		}
		if err := s.outbox.Enqueue(ctx, event); err != nil {
			return err
		}

		created = order
		return nil
	})
	if txErr != nil {
		return domain.Order{}, txErr
	}

	return created, nil
}

// resolveItems сопоставляет корзину с опубликованным меню, считает итоговую
// сумму по ценам из БД и собирает список списаний.
//
// Цены из запроса клиента не используются принципиально: иначе клиент мог бы
// назначить свою цену.
func resolveItems(
	snapshot domain.MenuSnapshot,
	draftItems []domain.DraftItem,
) (items []domain.OrderItem, reservations []reservation, subtotal int64, err error) {
	items = make([]domain.OrderItem, 0, len(draftItems))
	reservations = make([]reservation, 0, len(draftItems))

	for _, di := range draftItems {
		product, ok := snapshot.ProductByKey(di.ProductKey)
		if !ok {
			return nil, nil, 0, domain.Errorf(domain.CodeProductUnavailable,
				"позиции с ключом «%s» нет в текущем меню заведения", di.ProductKey)
		}
		if err := product.EnsureOrderable(); err != nil {
			return nil, nil, 0, err
		}

		lineTotal := domain.LineTotal(product.PriceKopecks, di.Qty)
		subtotal += lineTotal

		productID := product.ID
		items = append(items, domain.OrderItem{
			ProductID:           &productID,
			ProductKey:          product.ProductKey,
			ProductNameSnapshot: product.Name,
			UnitPriceKopecks:    product.PriceKopecks,
			Qty:                 di.Qty,
			LineTotalKopecks:    lineTotal,
		})

		if product.HasFiniteStock() {
			reservations = append(reservations, reservation{productID: product.ID, qty: di.Qty})
		}
	}

	return items, reservations, subtotal, nil
}

// Get отдаёт карточку заказа вместе с позициями и историей статусов.
func (s *OrderService) Get(ctx context.Context, publicNumber uuid.UUID) (domain.Order, error) {
	return s.orders.GetByPublicNumber(ctx, publicNumber)
}

// StatusChange — намерение перевести заказ в новый статус.
type StatusChange struct {
	PublicNumber uuid.UUID
	To           domain.OrderStatus
	Actor        domain.Actor
	Comment      string
	// Reason заполняется при отмене и попадает в orders.cancel_reason.
	Reason string
	// RestaurantID != 0 включает проверку принадлежности заказа заведению:
	// партнёр не должен управлять чужими заказами.
	RestaurantID int64
}

// ChangeStatus переводит заказ в новый статус.
//
// Внутри одной транзакции: проверка прав и допустимости перехода, возврат
// остатков (если переход отменяющий), обновление под оптимистичной блокировкой,
// запись события истории и, при необходимости, события в outbox.
func (s *OrderService) ChangeStatus(ctx context.Context, change StatusChange) (domain.Order, error) {
	var updated domain.Order

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		order, err := s.orders.GetByPublicNumber(ctx, change.PublicNumber)
		if err != nil {
			return err
		}

		if change.RestaurantID != 0 && order.RestaurantID != change.RestaurantID {
			// Намеренно не раскрываем существование чужого заказа деталями.
			return domain.Errorf(domain.CodeForbidden,
				"заказ %s не принадлежит вашему заведению", change.PublicNumber)
		}

		if err := domain.ValidateTransition(order.Status, change.To, change.Actor); err != nil {
			return err
		}

		if change.To.ReleasesStock() {
			if err := s.releaseStock(ctx, order); err != nil {
				return err
			}
		}

		var cancelReason *string
		if change.To.ReleasesStock() && change.Reason != "" {
			reason := change.Reason
			cancelReason = &reason
		}

		if err := s.orders.ApplyStatus(ctx, StatusUpdate{
			OrderID:         order.ID,
			ExpectedVersion: order.Version,
			FromStatus:      order.Status,
			ToStatus:        change.To,
			Actor:           change.Actor,
			Comment:         change.Comment,
			CancelReason:    cancelReason,
		}); err != nil {
			return err
		}

		if change.To.ReleasesStock() {
			order.Status = change.To
			event, err := newOrderCancelledEvent(order, change.Actor, change.Reason, s.now())
			if err != nil {
				return err
			}
			if err := s.outbox.Enqueue(ctx, event); err != nil {
				return err
			}
		}

		// Перечитываем заказ, чтобы вернуть клиенту гарантированно актуальные
		// version, updated_at и полный таймлайн.
		updated, err = s.orders.GetByPublicNumber(ctx, change.PublicNumber)
		return err
	})
	if err != nil {
		return domain.Order{}, err
	}

	return updated, nil
}

// CancelByUser — отмена заказа пользователем из веб-клиента.
func (s *OrderService) CancelByUser(ctx context.Context, publicNumber uuid.UUID, reason string) (domain.Order, error) {
	return s.ChangeStatus(ctx, StatusChange{
		PublicNumber: publicNumber,
		To:           domain.StatusCancelled,
		Actor:        domain.ActorUser,
		Comment:      reason,
		Reason:       reason,
	})
}

// releaseStock возвращает в каталог остатки, списанные при оформлении заказа.
//
// Позиции без product_id (удалённые из каталога) и позиции архивных версий меню
// пропускаются самим репозиторием — возвращать остаток в версию, которой больше
// нет на витрине, бессмысленно (ADR-0002).
func (s *OrderService) releaseStock(ctx context.Context, order domain.Order) error {
	items := append([]domain.OrderItem(nil), order.Items...)
	// Тот же детерминированный порядок блокировок, что и при списании.
	sort.Slice(items, func(i, j int) bool {
		if items[i].ProductID == nil {
			return false
		}
		if items[j].ProductID == nil {
			return true
		}
		return *items[i].ProductID < *items[j].ProductID
	})

	for _, item := range items {
		if item.ProductID == nil {
			continue
		}
		if err := s.menus.RestoreStock(ctx, *item.ProductID, item.Qty); err != nil {
			return err
		}
	}

	return nil
}
