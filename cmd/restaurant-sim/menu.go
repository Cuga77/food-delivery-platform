package main

// menuProduct — позиция эталонного меню заведения. Формат совпадает со схемой
// MenuSyncProduct из api/openapi.yaml: заведение отдаёт своё меню и платформе,
// и любому другому потребителю в одном и том же виде.
type menuProduct struct {
	ProductKey   string `json:"product_key"`
	Category     string `json:"category"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	PriceKopecks int64  `json:"price_kopecks"`
	Available    bool   `json:"available"`
	// StockQty == nil означает, что позиция всегда в наличии.
	StockQty *int32 `json:"stock_qty"`
}

func stock(qty int32) *int32 { return &qty }

// referenceMenu — «источник правды» заведения по своему ассортименту.
//
// Остатки намеренно разные: позиции без ограничения, с большим запасом и
// дефицитные — чтобы на демо-данных воспроизводились и успешный заказ, и
// OUT_OF_STOCK, и PRODUCT_UNAVAILABLE.
func referenceMenu() []menuProduct {
	return []menuProduct{
		{
			ProductKey:   "pizza_margherita",
			Category:     "Пицца",
			Name:         "Пицца Маргарита",
			Description:  "Томаты, моцарелла, свежий базилик",
			PriceKopecks: 59000,
			Available:    true,
			StockQty:     stock(10),
		},
		{
			ProductKey:   "pizza_pepperoni",
			Category:     "Пицца",
			Name:         "Пицца Пепперони",
			Description:  "Пепперони, моцарелла, томатный соус",
			PriceKopecks: 69000,
			Available:    true,
			StockQty:     stock(5),
		},
		{
			ProductKey:   "pizza_four_cheese",
			Category:     "Пицца",
			Name:         "Пицца 4 сыра",
			Description:  "Моцарелла, горгонзола, пармезан, чеддер",
			PriceKopecks: 75000,
			Available:    true,
			StockQty:     nil,
		},
		{
			ProductKey:   "pasta_carbonara",
			Category:     "Паста",
			Name:         "Паста Карбонара",
			Description:  "Спагетти, гуанчале, пармезан, желток",
			PriceKopecks: 49000,
			Available:    true,
			StockQty:     nil,
		},
		{
			ProductKey:   "salad_caesar",
			Category:     "Салаты",
			Name:         "Цезарь с курицей",
			Description:  "Романо, курица гриль, пармезан, гренки",
			PriceKopecks: 39000,
			Available:    true,
			StockQty:     stock(3),
		},
		{
			ProductKey:   "drink_cola",
			Category:     "Напитки",
			Name:         "Кола 0,5 л",
			Description:  "Охлаждённая",
			PriceKopecks: 12000,
			Available:    true,
			StockQty:     nil,
		},
		{
			ProductKey:   "dessert_tiramisu",
			Category:     "Десерты",
			Name:         "Тирамису",
			Description:  "Классический, с маскарпоне",
			PriceKopecks: 29000,
			// Позиция временно снята с продажи, но из меню не убрана: клиент
			// видит, что блюдо существует.
			Available: false,
			StockQty:  stock(0),
		},
	}
}
