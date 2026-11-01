package store

import "vyasa/src/models"

// tickerSectionTables lists the per-section tickertape tables, all of which
// share the same shape (see tickerSectionSchema).
func tickerSectionTables() []string {
	return []string{
		models.TableTickerCommentary,
		models.TableTickerFaq,
		models.TableTickerIncomeAnnual,
		models.TableTickerIncomeInterim,
		models.TableTickerBalancesheetAnnual,
		models.TableTickerCashflowAnnual,
		models.TableTickerPeers,
		models.TableTickerEventsCorpActions,
		models.TableTickerEventsAnnouncements,
		models.TableTickerEventsLegal,
	}
}
