// Package http — транспортный адаптер: реализация strict-сервера, полученного
// из api/openapi.yaml, поверх use cases из internal/app.
//
// Слой намеренно тонкий: разбор входа, вызов сценария, перевод результата в
// сгенерированные модели. Бизнес-правил здесь нет.
package http

import (
	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/gen"
)

// Домен и сгенерированные модели намеренно не совпадают: контракт может
// эволюционировать (новые поля, переименования) без переписывания доменных
// сущностей, а домен — меняться, не ломая клиентов. Цена — эти функции.

func toAPIRestaurant(r domain.Restaurant) gen.Restaurant {
	return gen.Restaurant{
		Id:                 r.ID,
		Slug:               r.Slug,
		Name:               r.Name,
		Status:             gen.RestaurantStatus(r.Status),
		MinOrderKopecks:    r.MinOrderKopecks,
		DeliveryFeeKopecks: r.DeliveryFeeKopecks,
	}
}

func toAPIRestaurantPage(page app.RestaurantPage) gen.RestaurantPage {
	// Индексный обход вместо range по значению: доменные сущности крупные,
	// и копировать каждую ради маппинга незачем.
	items := make([]gen.Restaurant, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, toAPIRestaurant(page.Items[i]))
	}

	result := gen.RestaurantPage{Items: items}
	if page.NextCursor != "" {
		cursor := page.NextCursor
		result.NextCursor = &cursor
	}

	return result
}

func toAPIProduct(p domain.Product) gen.Product {
	return gen.Product{
		ProductKey:   p.ProductKey,
		Category:     p.Category,
		Name:         p.Name,
		Description:  p.Description,
		PriceKopecks: p.PriceKopecks,
		Available:    p.Available,
		StockQty:     p.StockQty,
	}
}

func toAPIMenu(menu app.RestaurantMenu) gen.RestaurantMenu {
	categories := make([]gen.MenuCategory, 0, len(menu.Categories))
	for _, category := range menu.Categories {
		products := make([]gen.Product, 0, len(category.Products))
		for _, p := range category.Products {
			products = append(products, toAPIProduct(p))
		}
		categories = append(categories, gen.MenuCategory{
			Name:     category.Name,
			Products: products,
		})
	}

	return gen.RestaurantMenu{
		Restaurant:  toAPIRestaurant(menu.Restaurant),
		MenuVersion: menu.MenuVersion,
		Categories:  categories,
	}
}

func toAPIOrder(order domain.Order) gen.Order {
	// Пустые слайсы, а не nil: в контракте items и timeline обязательны, и
	// клиент должен получить [], а не null.
	items := make([]gen.OrderItem, 0, len(order.Items))
	for _, item := range order.Items {
		items = append(items, gen.OrderItem{
			ProductKey:       item.ProductKey,
			Name:             item.ProductNameSnapshot,
			UnitPriceKopecks: item.UnitPriceKopecks,
			Qty:              item.Qty,
			LineTotalKopecks: item.LineTotalKopecks,
		})
	}

	timeline := make([]gen.OrderStatusEvent, 0, len(order.Timeline))
	for _, event := range order.Timeline {
		timeline = append(timeline, gen.OrderStatusEvent{
			FromStatus: string(event.FromStatus),
			ToStatus:   string(event.ToStatus),
			Actor:      gen.Actor(event.Actor),
			Comment:    event.Comment,
			CreatedAt:  event.CreatedAt,
		})
	}

	return gen.Order{
		PublicNumber:       order.PublicNumber,
		RestaurantId:       order.RestaurantID,
		RestaurantSlug:     order.RestaurantSlug,
		UserExternalId:     order.UserExternalID,
		Status:             gen.OrderStatus(order.Status),
		DeliveryAddress:    order.DeliveryAddress,
		Items:              items,
		SubtotalKopecks:    order.SubtotalKopecks,
		DeliveryFeeKopecks: order.DeliveryFeeKopecks,
		TotalKopecks:       order.TotalKopecks,
		CancelReason:       order.CancelReason,
		CreatedAt:          order.CreatedAt,
		UpdatedAt:          order.UpdatedAt,
		Timeline:           timeline,
	}
}

func toAPIOrders(orders []domain.Order) []gen.Order {
	result := make([]gen.Order, 0, len(orders))
	for i := range orders {
		result = append(result, toAPIOrder(orders[i]))
	}
	return result
}

func toDomainDraft(body gen.CreateOrderRequest) domain.OrderDraft {
	items := make([]domain.DraftItem, 0, len(body.Items))
	for _, item := range body.Items {
		items = append(items, domain.DraftItem{
			ProductKey: item.ProductKey,
			Qty:        item.Qty,
		})
	}

	return domain.OrderDraft{
		UserExternalID:  body.UserExternalId,
		RestaurantID:    body.RestaurantId,
		DeliveryAddress: body.DeliveryAddress,
		Items:           items,
	}
}

func toDomainSyncProducts(body gen.MenuSyncRequest) []domain.Product {
	products := make([]domain.Product, 0, len(body.Products))

	for _, p := range body.Products {
		// available и description необязательны в контракте: по умолчанию
		// позиция доступна, а описание пустое.
		available := true
		if p.Available != nil {
			available = *p.Available
		}
		description := ""
		if p.Description != nil {
			description = *p.Description
		}

		products = append(products, domain.Product{
			ProductKey:   p.ProductKey,
			Category:     p.Category,
			Name:         p.Name,
			Description:  description,
			PriceKopecks: p.PriceKopecks,
			Available:    available,
			StockQty:     p.StockQty,
		})
	}

	return products
}
