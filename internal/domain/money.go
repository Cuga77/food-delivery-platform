package domain

import "fmt"

// Деньги в системе — всегда целое число копеек (int64). Плавающая точка не
// используется нигде: ни в контракте, ни в БД, ни в расчётах.

// LineTotal считает стоимость позиции. Отдельная функция, чтобы правило
// «цена × количество» жило в одном месте и совпадало с CHECK-ограничением
// order_items_line_total_matches в БД.
func LineTotal(unitPriceKopecks int64, qty int32) int64 {
	return unitPriceKopecks * int64(qty)
}

// FormatKopecks приводит копейки к человекочитаемому виду для текстов ошибок.
func FormatKopecks(kopecks int64) string {
	sign := ""
	if kopecks < 0 {
		sign = "-"
		kopecks = -kopecks
	}
	return fmt.Sprintf("%s%d,%02d ₽", sign, kopecks/100, kopecks%100)
}
