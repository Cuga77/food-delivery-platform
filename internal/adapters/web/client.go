package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

const restaurantsPageSize = 20

// myOrdersPath — история заказов текущего браузера.
const myOrdersPath = "/orders"

// --- Каталог ----------------------------------------------------------------

type restaurantsPageData struct {
	Title       string
	Restaurants []domain.Restaurant
	NextCursor  string
}

// restaurantsPage — витрина каталога.
//
// Для htmx отдаётся только фрагмент с карточками и новой кнопкой «показать
// ещё»: подгрузка страницы дописывает карточки, а не вкладывает целый документ
// внутрь блока.
func (h *Handler) restaurantsPage(w http.ResponseWriter, r *http.Request) {
	page, err := h.catalog.ListRestaurants(r.Context(), app.ListRestaurantsQuery{
		Limit:  restaurantsPageSize,
		Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		h.renderError(w, r, err, backLink{URL: "/restaurants", Label: "Обновить"})
		return
	}

	data := restaurantsPageData{
		Title:       "Заведения",
		Restaurants: page.Items,
		NextCursor:  page.NextCursor,
	}

	if isHTMX(r) {
		h.render(w, r, http.StatusOK, partialRestaurantCards, data)
		return
	}
	h.render(w, r, http.StatusOK, pageRestaurants, data)
}

// --- Меню -------------------------------------------------------------------

type menuPageData struct {
	Title      string
	Restaurant domain.Restaurant
	Categories []domain.MenuCategory
	Version    int32
	// IdempotencyKey кладётся в скрытое поле формы: повторная отправка той же
	// формы (кнопка «назад», двойной клик) не создаст второй заказ.
	IdempotencyKey string
	AcceptsOrders  bool
}

func (h *Handler) menuPage(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	menu, err := h.catalog.GetMenuBySlug(r.Context(), slug)
	if err != nil {
		h.renderError(w, r, err, backToRestaurants)
		return
	}

	// Личность посетителя выдаётся здесь, вместе с формой заказа, а не при её
	// отправке. Иначе двойной клик по кнопке «оформить» — тот самый сценарий,
	// ради которого существует идемпотентность, — приходил бы двумя запросами
	// без cookie, каждый получал бы собственный случайный идентификатор, и
	// вместо повтора посетитель видел бы ошибку расхождения содержимого.
	if _, err := h.visitorID(w, r); err != nil {
		h.renderError(w, r, domain.Errorf(domain.CodeInternalError,
			"не удалось начать сессию — попробуйте ещё раз"), backToRestaurants)
		return
	}

	h.render(w, r, http.StatusOK, pageMenu, menuPageData{
		Title:          menu.Restaurant.Name,
		Restaurant:     menu.Restaurant,
		Categories:     menu.Categories,
		Version:        menu.MenuVersion,
		IdempotencyKey: uuid.NewString(),
		AcceptsOrders:  menu.Restaurant.Status.AcceptsOrders(),
	})
}

// --- Оформление заказа ------------------------------------------------------

// qtyFieldPrefix — префикс полей количества в форме меню: qty_<product_key>.
//
// Артикул в имени поля, а не порядковый номер, избавляет от синхронизации
// индексов на клиенте: форма отправляет ровно то, что видел человек, и
// работает без JavaScript.
const qtyFieldPrefix = "qty_"

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.badRequest(w, r, "не удалось разобрать форму заказа", backToRestaurants)
		return
	}

	restaurantID, err := strconv.ParseInt(r.PostFormValue("restaurant_id"), 10, 64)
	if err != nil || restaurantID <= 0 {
		h.badRequest(w, r, "не указано заведение", backToRestaurants)
		return
	}

	back := backLink{URL: safeReturnPath(r.PostFormValue("back_url")), Label: "Вернуться к меню"}
	if back.URL == "" {
		back = backToRestaurants
	}

	visitor, err := h.visitorID(w, r)
	if err != nil {
		h.renderError(w, r, domain.Errorf(domain.CodeInternalError,
			"не удалось начать сессию — попробуйте ещё раз"), back)
		return
	}

	draft := domain.OrderDraft{
		UserExternalID:  visitor,
		RestaurantID:    restaurantID,
		DeliveryAddress: strings.TrimSpace(r.PostFormValue("delivery_address")),
		Items:           cartFromForm(r),
	}

	if len(draft.Items) == 0 {
		h.badRequest(w, r, "Выберите хотя бы одно блюдо — корзина пуста.", back)
		return
	}

	// Идемпотентность реализована здесь, а не общей прослойкой: у формы нет
	// возможности передать заголовок Idempotency-Key, а повтор должен вести на
	// уже созданный заказ, а не отдавать сохранённое JSON-тело.
	key := r.PostFormValue("idempotency_key")
	if key == "" {
		h.badRequest(w, r, "форма устарела, откройте меню заново", back)
		return
	}

	existingNumber, claimed, err := h.claimOrderForm(ctx, key, draft)
	if err != nil {
		h.renderError(w, r, err, back)
		return
	}
	if !claimed {
		// Заказ по этой форме уже оформлен — показываем его.
		existing, parseErr := uuid.Parse(existingNumber)
		if parseErr != nil {
			h.renderError(w, r, domain.Errorf(domain.CodeInternalError,
				"не удалось открыть ранее оформленный заказ"), back)
			return
		}
		//nolint:gosec // G710: путь построен из разобранного UUID, см. orderPath
		http.Redirect(w, r, orderPath(existing), http.StatusSeeOther)
		return
	}

	order, err := h.orders.Create(ctx, draft)
	if err != nil {
		h.releaseOrderForm(ctx, key)
		h.renderError(w, r, err, back)
		return
	}

	h.completeOrderForm(ctx, key, order.PublicNumber.String())

	// Post/Redirect/Get: обновление страницы результата не повторяет отправку.
	http.Redirect(w, r, orderPath(order.PublicNumber), http.StatusSeeOther)
}

// orderPath собирает адрес карточки заказа из разобранного UUID.
//
// Аргумент — значение uuid.UUID, а не строка из запроса: String() возвращает
// каноничные 36 hex-символов с дефисами, поэтому подставить в путь внешний
// адрес невозможно. Анализатор taint-потока этого не видит и помечает вызовы
// как открытый редирект — отсюда nolint в местах вызова.
func orderPath(number uuid.UUID) string {
	return "/orders/" + number.String()
}

// cartFromForm собирает корзину из полей qty_<product_key>.
//
// Порядок позиций детерминирован (по артикулу): при равном наборе блюд тело
// заказа одинаково, а значит совпадает и хэш формы для идемпотентности.
func cartFromForm(r *http.Request) []domain.DraftItem {
	items := make([]domain.DraftItem, 0, len(r.PostForm))

	for field, values := range r.PostForm {
		if !strings.HasPrefix(field, qtyFieldPrefix) || len(values) == 0 {
			continue
		}

		productKey := strings.TrimPrefix(field, qtyFieldPrefix)
		if productKey == "" {
			continue
		}

		qty, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 32)
		if err != nil || qty <= 0 {
			continue
		}

		items = append(items, domain.DraftItem{ProductKey: productKey, Qty: int32(qty)})
	}

	sort.Slice(items, func(i, j int) bool { return items[i].ProductKey < items[j].ProductKey })
	return items
}

// safeReturnPath пропускает только относительный путь внутри этого сайта.
//
// Значение приходит из формы и попадает в href кнопки «вернуться» на странице
// ошибки. Без проверки это готовый фишинговый рычаг: злоумышленник подсовывает
// ссылку, жертва видит настоящую страницу сервиса с кнопкой, ведущей на чужой
// сайт. html/template обезвреживает схему javascript:, но внешний https-адрес
// пропускает — от подмены адреса шаблонизатор не защищает.
//
// Пустая строка означает «использовать умолчание вызывающего».
func safeReturnPath(raw string) string {
	// Путь обязан начинаться с одного слэша. Два слэша — это адрес вида
	// //evil.example.com, который браузер трактует как внешний сайт.
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}

	// Обратный слэш браузеры местами приравнивают к прямому: /\evil.example.com
	// уводит на чужой домен.
	if strings.HasPrefix(raw, "/\\") || strings.ContainsAny(raw, "\\\r\n") {
		return ""
	}

	// Разбор отсекает адреса вроде /path\x00 и подтверждает, что схемы и хоста
	// в значении нет.
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return ""
	}

	return parsed.String()
}

// --- Идемпотентность формы заказа -------------------------------------------

// webIdempotencyPrefix отделяет ключи веб-форм от ключей JSON API: это разные
// пространства имён, и пересечение значений не должно приводить к ложному
// повтору.
const webIdempotencyPrefix = "web:"

// storedOrder — что сохраняется под ключом формы.
type storedOrder struct {
	PublicNumber string `json:"public_number"`
}

// claimOrderForm захватывает ключ формы.
//
// Возвращает (номер уже созданного заказа, claimed=false), если форму с этим
// ключом и этим содержимым уже отправляли.
func (h *Handler) claimOrderForm(
	ctx context.Context,
	key string,
	draft domain.OrderDraft,
) (existingNumber string, claimed bool, err error) {
	record, claimed, err := h.idempotency.Reserve(ctx,
		webIdempotencyPrefix+key, draftHash(draft), time.Now().Add(h.idempotencyTTL))
	if err != nil {
		return "", false, err
	}
	if claimed {
		return "", true, nil
	}

	switch {
	case record.Key == "":
		// Запись исчезла между захватом и чтением — редкая гонка с уборкой.
		return "", false, domain.Errorf(domain.CodeIdempotencyInProgress,
			"заказ по этой форме сейчас оформляется, обновите страницу через несколько секунд")

	case record.RequestHash != draftHash(draft):
		return "", false, domain.Errorf(domain.CodeIdempotencyPayloadMismatch,
			"состав заказа изменился — вернитесь в меню и соберите корзину заново")

	case record.InProgress():
		return "", false, domain.Errorf(domain.CodeIdempotencyInProgress,
			"заказ по этой форме сейчас оформляется, обновите страницу через несколько секунд")
	}

	var stored storedOrder
	if err := json.Unmarshal(record.ResponseBody, &stored); err != nil {
		return "", false, domain.Errorf(domain.CodeInternalError,
			"не удалось восстановить ранее оформленный заказ")
	}

	// Значение пришло из хранилища, но перед подстановкой в адрес всё равно
	// проверяется: в URL попадает только каноничный UUID, а не произвольная
	// строка из базы.
	number, parseErr := uuid.Parse(stored.PublicNumber)
	if parseErr != nil {
		return "", false, domain.Errorf(domain.CodeInternalError,
			"не удалось восстановить ранее оформленный заказ")
	}

	return number.String(), false, nil
}

func (h *Handler) completeOrderForm(ctx context.Context, key, publicNumber string) {
	body, err := json.Marshal(storedOrder{PublicNumber: publicNumber})
	if err != nil {
		return
	}

	// Контекст запроса к этому моменту может быть уже отменён: клиент получил
	// редирект и отсоединился. Записать результат нужно всё равно, иначе ключ
	// останется «выполняющимся» до конца TTL, и повтор формы получит отказ
	// вместо ссылки на созданный заказ.
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	if err := h.idempotency.Complete(completeCtx, webIdempotencyPrefix+key, http.StatusSeeOther, body); err != nil {
		// Заказ создан и пользователь его увидит; повтор формы в худшем случае
		// выполнится заново. Логируем и не мешаем ответу.
		h.log.ErrorContext(completeCtx, "не удалось сохранить результат формы заказа",
			slog.String("idempotency_key", key), slog.Any("error", err))
	}
}

func (h *Handler) releaseOrderForm(ctx context.Context, key string) {
	// Контекст запроса может быть уже отменён, а захват снять нужно — иначе
	// пользователь не сможет повторить отправку после исправления корзины.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	if err := h.idempotency.Release(releaseCtx, webIdempotencyPrefix+key); err != nil {
		h.log.ErrorContext(releaseCtx, "не удалось освободить ключ формы заказа",
			slog.String("idempotency_key", key), slog.Any("error", err))
	}
}

// draftHash считает отпечаток содержимого формы: тот же ключ с другой корзиной
// должен распознаваться как ошибка, а не как повтор.
func draftHash(draft domain.OrderDraft) string {
	sum := sha256.New()

	// Идентификатор посетителя в хэш намеренно не входит: хэш отвечает на
	// вопрос «это та же самая отправка формы?», а личность частью формы не
	// является. Если браузер запретил cookie, повторная отправка приходит с
	// новым случайным идентификатором — и, войди он в хэш, посетитель вместо
	// повтора получил бы ошибку расхождения содержимого. Разделение с ключами
	// JSON API обеспечено префиксом пространства имён, а не хэшем; сам ключ
	// выдаётся при рендере меню и живёт внутри одной сессии оформления.
	sum.Write([]byte(strconv.FormatInt(draft.RestaurantID, 10)))
	sum.Write([]byte{0})
	sum.Write([]byte(draft.DeliveryAddress))

	for _, item := range draft.Items {
		sum.Write([]byte{0})
		sum.Write([]byte(item.ProductKey))
		sum.Write([]byte{0})
		sum.Write([]byte(strconv.FormatInt(int64(item.Qty), 10)))
	}

	return hex.EncodeToString(sum.Sum(nil))
}

// --- История заказов --------------------------------------------------------

// userOrdersPageSize — размер страницы истории.
const userOrdersPageSize = 20

type myOrdersPageData struct {
	Title      string
	Orders     []domain.Order
	NextCursor string
	// KnownVisitor == false означает, что браузер ещё ничего не заказывал:
	// пустой список тогда нужно объяснить, а не показать как «заказов нет».
	KnownVisitor bool
}

// myOrdersPage — история заказов текущего браузера.
//
// Идентификатор берётся из cookie и никогда из запроса: приняв его из query,
// страница превратилась бы в чтение чужой истории по подставленному значению.
// Новая личность здесь не выдаётся — посетителю, который ещё ничего не
// заказывал, показывать нечего, и заводить ради этого cookie не нужно.
func (h *Handler) myOrdersPage(w http.ResponseWriter, r *http.Request) {
	visitor := currentVisitorID(r)
	if visitor == "" {
		h.render(w, r, http.StatusOK, pageMyOrders, myOrdersPageData{
			Title: "Мои заказы",
		})
		return
	}

	page, err := h.orders.ListByUser(r.Context(), app.ListUserOrdersQuery{
		UserExternalID: visitor,
		Limit:          userOrdersPageSize,
		Cursor:         r.URL.Query().Get("cursor"),
	})
	if err != nil {
		h.renderError(w, r, err, backLink{URL: myOrdersPath, Label: "Обновить"})
		return
	}

	data := myOrdersPageData{
		Title:        "Мои заказы",
		Orders:       page.Items,
		NextCursor:   page.NextCursor,
		KnownVisitor: true,
	}

	if isHTMX(r) {
		h.render(w, r, http.StatusOK, partialMyOrderRows, data)
		return
	}
	h.render(w, r, http.StatusOK, pageMyOrders, data)
}

// --- Карточка заказа --------------------------------------------------------

type orderPageData struct {
	Title string
	Order domain.Order
	// CanCancel — показывать ли форму отмены. Правило одно и то же, что и в
	// домене: пока заведение не приняло заказ.
	CanCancel bool
	// Live — нужно ли опрашивать статус: у завершённого заказа обновлять нечего.
	Live bool
}

func (h *Handler) orderPage(w http.ResponseWriter, r *http.Request) {
	publicNumber, err := uuid.Parse(chi.URLParam(r, "public_number"))
	if err != nil {
		h.badRequest(w, r, "некорректный номер заказа", backToRestaurants)
		return
	}

	order, err := h.orders.Get(r.Context(), publicNumber)
	if err != nil {
		h.renderError(w, r, err, backToRestaurants)
		return
	}

	data := orderPageData{
		Title:     "Заказ " + shortID(order.PublicNumber.String()),
		Order:     order,
		CanCancel: domain.CanTransition(order.Status, domain.StatusCancelled, domain.ActorUser),
		Live:      !order.Status.IsTerminal(),
	}

	if isHTMX(r) {
		h.render(w, r, http.StatusOK, partialOrderDetail, data)
		return
	}
	h.render(w, r, http.StatusOK, pageOrder, data)
}

func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	publicNumberStr := chi.URLParam(r, "public_number")

	publicNumber, err := uuid.Parse(publicNumberStr)
	if err != nil {
		h.badRequest(w, r, "некорректный номер заказа", backToRestaurants)
		return
	}

	if err := r.ParseForm(); err != nil {
		h.badRequest(w, r, "не удалось разобрать форму", backToRestaurants)
		return
	}

	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		reason = "Отменено пользователем"
	}

	back := backLink{URL: "/orders/" + publicNumberStr, Label: "Вернуться к заказу"}

	if _, err := h.orders.CancelByUser(r.Context(), publicNumber, reason); err != nil {
		h.renderError(w, r, err, back)
		return
	}

	//nolint:gosec // G710: путь построен из разобранного UUID, см. orderPath
	http.Redirect(w, r, orderPath(publicNumber), http.StatusSeeOther)
}
