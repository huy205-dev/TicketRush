package order

// Status is an order state (SPEC.md section 8).
type Status string

const (
	StatusHeld      Status = "HELD"
	StatusPaid      Status = "PAID"
	StatusTicketed  Status = "TICKETED"
	StatusExpired   Status = "EXPIRED"
	StatusCancelled Status = "CANCELLED"
	StatusRefunding Status = "REFUNDING"
	StatusRefunded  Status = "REFUNDED"
)

// AllStatuses lists every status, in the order of SPEC.md section 8.
var AllStatuses = []Status{
	StatusHeld, StatusPaid, StatusTicketed, StatusExpired,
	StatusCancelled, StatusRefunding, StatusRefunded,
}

// transitions is the complete state machine. Anything not listed is illegal.
var transitions = map[Status][]Status{
	StatusHeld: {
		StatusPaid,      // payment on time with the right amount
		StatusCancelled, // buyer cancels
		StatusExpired,   // expiry worker
		StatusRefunding, // payment with the wrong amount
	},
	StatusPaid: {
		StatusTicketed, // ticket service issued the QR codes
	},
	StatusExpired: {
		StatusPaid,      // late payment, seats could be re-held
		StatusRefunding, // late payment, seats are gone
	},
	StatusCancelled: {
		StatusRefunding, // payment arrived after cancelling
	},
	StatusRefunding: {
		StatusRefunded, // fakepay confirmed the refund
	},
	StatusTicketed: nil,
	StatusRefunded: nil,
}

// CanTransition reports whether an order may move from one status to another.
func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}
