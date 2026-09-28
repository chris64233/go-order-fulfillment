package goorderfulfillment

import "sort"

// ReportPicking 接收某个仓库对某版方案的拣货回执。
//
// 语义：
//   - 只允许回执“当前仍开放”的分配行。旧方案中已因短缺而关闭、已取消的分配行
//     再来回执一律以 ErrStaleReceipt 拒绝——迟到回执既不能覆盖新分配，
//     也不可能让同一商品被两个仓重复发出。
//   - 拣出数量累计保留（可多次补拣）：拣出一件，OnHand 与 Reserved 各减一。
//   - 条目显式带 Short=true 才表示报告短缺：已拣部分保留，未拣部分核销并释放；
//     回执中未出现的分配行仍处于待拣状态，不做任何推断。
//   - 出现短缺且因此产生缺口时，立刻尝试用各仓当前 ATP 生成下一版跨仓方案；
//     现有库存仍无法补齐缺口则保留缺口，订单进入 awaiting_stock，等待
//     RegisterStock 后调用 Reallocate。
//   - ReceiptID 非空时做回执级幂等去重。
func (svc *Service) ReportPicking(r PickingReceipt) (*PickingResult, error) {
	if r.OrderNumber == "" || r.WarehouseID == "" {
		return nil, ErrInvalidInput
	}
	type lineIn struct {
		picked int
		short  bool
	}
	// 合并同 SKU 条目并校验数量；重复声明中任一 Short=true 即视为声明短缺。
	got := map[SKU]lineIn{}
	for _, it := range r.Items {
		if it.SKU == "" || it.PickedQuantity < 0 {
			return nil, ErrInvalidInput
		}
		cur := got[it.SKU]
		got[it.SKU] = lineIn{picked: cur.picked + it.PickedQuantity, short: cur.short || it.Short}
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()

	o, err := svc.st.getOrder(r.OrderNumber)
	if err != nil {
		return nil, err
	}
	if o.Status == OrderCancelled {
		return nil, ErrOrderTerminal
	}
	if r.ReceiptID != "" {
		if _, dup := svc.st.seenReceipts[receiptKey{r.OrderNumber, r.ReceiptID}]; dup {
			return nil, ErrReceiptDuplicate
		}
	}
	plan, err := svc.st.getPlan(r.OrderNumber, r.PlanVersion)
	if err != nil {
		return nil, err
	}

	// 阶段一：校验。所有条目都必须落在该仓、该版、当前仍开放的分配行上，
	// 且本次拣出数量不得超过尚未拣出的需求；任一不合法则整批回执不生效。
	type apply struct {
		alloc  *Allocation
		picked int
		short  bool
	}
	applies := make([]apply, 0, len(got))
	for sku, in := range got {
		a := findAllocation(plan, r.WarehouseID, sku)
		if a == nil {
			return nil, ErrAllocationNotFound
		}
		if !a.Open() {
			return nil, ErrStaleReceipt
		}
		if in.picked > a.Quantity-a.PickedQuantity {
			return nil, ErrOverPicked
		}
		applies = append(applies, apply{alloc: a, picked: in.picked, short: in.short})
	}

	// 幂等键在通过全部校验后才登记，避免失败的回执“占坑”。
	if r.ReceiptID != "" {
		svc.st.seenReceipts[receiptKey{r.OrderNumber, r.ReceiptID}] = struct{}{}
	}

	// 阶段二：应用拣货结果。
	var shortages []*Allocation
	for _, ap := range applies {
		a := ap.alloc
		if ap.picked > 0 {
			st := svc.st.getStock(a.WarehouseID, a.SKU, false)
			if st == nil {
				// 不可达：开放分配必然来自已登记库存；防御性处理。
				return nil, ErrInvalidInput
			}
			st.OnHand -= ap.picked
			st.Reserved -= ap.picked
			a.PickedQuantity += ap.picked
		}
		switch {
		case a.PickedQuantity == a.Quantity:
			a.Status = AllocPicked
		case ap.short:
			a.Status = AllocShortage
			shortages = append(shortages, a)
		case a.PickedQuantity > 0:
			a.Status = AllocPartial
		}
	}
	for _, a := range shortages {
		// 短缺部分视为账实差异核销：未拣数量既释放占用，也从实物库存中扣减，
		// 否则同一缺口会被立刻重新分配回这个明确报告“拿不出货”的仓库。
		// 货物事后找回时，可通过 RegisterStock 重新入库。
		st := svc.st.getStock(a.WarehouseID, a.SKU, false)
		if st != nil {
			shortfall := a.Quantity - a.PickedQuantity
			st.Reserved -= shortfall
			st.OnHand -= shortfall
			if st.Reserved < 0 {
				st.Reserved = 0
			}
			if st.OnHand < 0 {
				st.OnHand = 0
			}
		}
	}

	svc.finalizePlanLocked(plan)
	var newPlan *AllocationPlan
	if len(shortages) > 0 {
		newPlan, err = svc.createReplanIfGapLocked(o)
		if err != nil && err != ErrNothingToReallocate && err != ErrInsufficientStock {
			// 理论不可达：保持状态原样并返回。
			return nil, err
		}
		err = nil
	}
	o.Status = svc.deriveStatusLocked(o)

	if perr := svc.saved(); perr != nil {
		// 持久化失败：内存状态已经变更，返回错误让调用方感知；
		// 锁保护下没有其他写者会观察到半状态。
		return nil, perr
	}

	res := &PickingResult{Status: o.Status}
	if newPlan != nil {
		res.NewPlan = clonePlan(newPlan)
	}
	return res, nil
}

// PickingResult 是拣货回执处理结果。
type PickingResult struct {
	// NewPlan 当短缺触发了下一版跨仓方案时非空。
	NewPlan *AllocationPlan
	// Status 处理后的订单状态。
	Status OrderStatus
}

// Reallocate 为订单当前的缺货缺口主动生成下一版跨仓方案。
//
// 使用场景：上一轮重配时 ATP 不足（订单处于 awaiting_stock），之后仓库补货
// （RegisterStock），调用方再次尝试分配。没有缺口时返回 ErrNothingToReallocate；
// 现有库存仍不足时返回 ErrInsufficientStock，已拣货与既有分配不受影响。
func (svc *Service) Reallocate(orderNo OrderNumber) (*AllocationPlan, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	o, err := svc.st.getOrder(orderNo)
	if err != nil {
		return nil, err
	}
	if o.Status == OrderCancelled {
		return nil, ErrOrderTerminal
	}
	plan, err := svc.createReplanIfGapLocked(o)
	if err != nil {
		return nil, err
	}
	o.Status = svc.deriveStatusLocked(o)
	if err := svc.saved(); err != nil {
		return nil, err
	}
	return clonePlan(plan), nil
}

// createReplanIfGapLocked 计算缺口；缺口为 0 返回 ErrNothingToReallocate。
// 成功时追加新版方案，并把所有“带缺口且尚未被替代”的旧版方案标记为 superseded。
func (svc *Service) createReplanIfGapLocked(o *Order) (*AllocationPlan, error) {
	gap := svc.shortageGapLocked(o.OrderNumber)
	hasGap := false
	for _, q := range gap {
		if q > 0 {
			hasGap = true
			break
		}
	}
	if !hasGap {
		return nil, ErrNothingToReallocate
	}
	nextVersion := len(svc.st.plans[o.OrderNumber]) + 1
	lines := make([]OrderLine, 0, len(gap))
	for sku, q := range gap {
		lines = append(lines, OrderLine{SKU: sku, Quantity: q})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].SKU < lines[j].SKU })

	plan, err := svc.buildPlanLocked(o.OrderNumber, lines, nextVersion)
	if err != nil {
		return nil, err
	}
	svc.st.appendPlan(plan)

	// 任何仍挂着缺口、尚未被其他版本承接的旧方案都由本版承接。
	for _, old := range svc.st.allPlans(o.OrderNumber) {
		if old.Version == nextVersion {
			continue
		}
		if old.Status == PlanActive && svc.planHasShortageLocked(o.OrderNumber, old) {
			old.Status = PlanSuperseded
			old.SupersededBy = nextVersion
		}
	}
	return plan, nil
}

// shortageGapLocked 按 SKU 计算“订单需求 - 已拣出 - 仍开放分配中的数量”。
// 开放分配按整件数量（含其已拣部分）作为在途承诺扣减；已关闭（短缺/取消）
// 的分配只扣减成功拣出量。因此已关闭缺口不会被重复分配。
func (svc *Service) shortageGapLocked(orderNo OrderNumber) map[SKU]int {
	o := svc.st.orders[orderNo]
	gap := map[SKU]int{}
	for _, l := range o.Lines {
		gap[l.SKU] = l.Quantity
	}
	for _, p := range svc.st.allPlans(orderNo) {
		for _, a := range p.Allocations {
			if a.Open() {
				gap[a.SKU] -= a.Quantity
			} else {
				gap[a.SKU] -= a.PickedQuantity
			}
		}
	}
	for sku := range gap {
		if gap[sku] < 0 {
			gap[sku] = 0
		}
	}
	return gap
}

// planHasShortageLocked 判断某版方案是否含“关闭的短缺分配行”。
func (svc *Service) planHasShortageLocked(_ OrderNumber, p *AllocationPlan) bool {
	for _, a := range p.Allocations {
		if a.Status == AllocShortage {
			return true
		}
	}
	return false
}

// finalizePlanLocked 在回执处理后重算单版方案状态。
func (svc *Service) finalizePlanLocked(p *AllocationPlan) {
	// 已被新版替代或已取消的方案，迟到回执不得把它“降级”回活跃态。
	if p.Status == PlanSuperseded || p.Status == PlanCancelled {
		return
	}
	open, picked, shortage := 0, 0, 0
	for _, a := range p.Allocations {
		switch {
		case a.Open():
			open++
		case a.Status == AllocPicked:
			picked++
		case a.Status == AllocShortage:
			shortage++
		}
	}
	switch {
	case open > 0:
		p.Status = PlanActive // 仍有仓库未回执
	case shortage == 0:
		p.Status = PlanCompleted // 全部拣满
	default:
		// 全量关闭且含缺口：保持 active 表示“挂缺口”。它的分配行均已关闭、
		// 不占用库存；待 createReplanIfGapLocked 成功后转为 superseded，
		// 补货始终不足则一直处于该挂起态。
		p.Status = PlanActive
	}
}

// deriveStatusLocked 依据各版方案与发货记录重算订单整体状态。
func (svc *Service) deriveStatusLocked(o *Order) OrderStatus {
	if o.Status == OrderCancelled {
		return OrderCancelled
	}
	picked := map[SKU]int{}
	openQty := map[SKU]int{}
	for _, p := range svc.st.allPlans(o.OrderNumber) {
		for _, a := range p.Allocations {
			picked[a.SKU] += a.PickedQuantity
			if a.Open() {
				openQty[a.SKU] += a.Quantity - a.PickedQuantity
			}
		}
	}
	shipped := map[SKU]int{}
	for _, sh := range svc.st.shipments[o.OrderNumber] {
		for _, it := range sh.Items {
			shipped[it.SKU] += it.Quantity
		}
	}

	totalShipped, totalRequired := 0, 0
	allShipped, anyShipped := true, false
	allPicked, anyOpen, anyGap := true, false, false
	for _, l := range o.Lines {
		totalRequired += l.Quantity
		totalShipped += shipped[l.SKU]
		if shipped[l.SKU] < l.Quantity {
			allShipped = false
		}
		if shipped[l.SKU] > 0 {
			anyShipped = true
		}
		if picked[l.SKU] < l.Quantity {
			allPicked = false
		}
		if openQty[l.SKU] > 0 {
			anyOpen = true
		}
		if picked[l.SKU]+openQty[l.SKU] < l.Quantity {
			anyGap = true
		}
	}
	switch {
	case allShipped:
		return OrderShipped
	case anyShipped && totalShipped < totalRequired:
		return OrderPartiallyShipped
	case allPicked:
		return OrderPickedComplete
	case anyGap && !anyOpen:
		// 有缺口但没有任何在途分配，只能等待补货重配。
		return OrderAwaitingStock
	case anyGap:
		// 既有在途分配、又有缺口（新版重配可能只补了一部分）。
		return OrderAwaitingStock
	default:
		return OrderConfirmed
	}
}

// ---------- 发货 ----------

// ConfirmShipment 确认某仓从已拣货库存中发出商品。
//
// 发货数量不得超过该仓“已拣未发”数量，因此同一商品不可能被两个仓重复发出：
// 拣货量受分配需求约束，发货量又受拣货量约束，且全程在同一把锁内校验与记账。
// ShipmentID 非空时做发货级幂等去重。
func (svc *Service) ConfirmShipment(s Shipment) (*ShipmentResult, error) {
	if s.OrderNumber == "" || s.WarehouseID == "" || len(s.Items) == 0 {
		return nil, ErrInvalidInput
	}
	items := map[SKU]int{}
	for _, it := range s.Items {
		if it.SKU == "" || it.Quantity <= 0 {
			return nil, ErrInvalidInput
		}
		items[it.SKU] += it.Quantity
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()

	o, err := svc.st.getOrder(s.OrderNumber)
	if err != nil {
		return nil, err
	}
	if o.Status == OrderCancelled {
		return nil, ErrOrderTerminal
	}
	if s.ShipmentID != "" {
		if _, dup := svc.st.seenShipments[shipmentKey{s.OrderNumber, s.ShipmentID}]; dup {
			return nil, ErrShipmentDuplicate
		}
	}

	// 该仓每个 SKU 的已拣总量与已发总量。
	pickedByWH := map[SKU]int{}
	shippedByWH := map[SKU]int{}
	for _, p := range svc.st.allPlans(s.OrderNumber) {
		for _, a := range p.Allocations {
			if a.WarehouseID == s.WarehouseID {
				pickedByWH[a.SKU] += a.PickedQuantity
			}
		}
	}
	for _, sh := range svc.st.shipments[s.OrderNumber] {
		if sh.WarehouseID == s.WarehouseID {
			for _, it := range sh.Items {
				shippedByWH[it.SKU] += it.Quantity
			}
		}
	}
	for sku, qty := range items {
		if qty > pickedByWH[sku]-shippedByWH[sku] {
			return nil, ErrOverShipped
		}
	}
	// 订单维度总量也不得超过订单需求。
	totalShipped := map[SKU]int{}
	for _, sh := range svc.st.shipments[s.OrderNumber] {
		for _, it := range sh.Items {
			totalShipped[it.SKU] += it.Quantity
		}
	}
	req := map[SKU]int{}
	for _, l := range o.Lines {
		req[l.SKU] = l.Quantity
	}
	for sku, qty := range items {
		if totalShipped[sku]+qty > req[sku] {
			return nil, ErrOverShipped
		}
	}

	rec := &Shipment{
		ShipmentID:  s.ShipmentID,
		OrderNumber: s.OrderNumber,
		WarehouseID: s.WarehouseID,
	}
	for sku, qty := range items {
		rec.Items = append(rec.Items, ShipmentItem{SKU: sku, Quantity: qty})
	}
	sort.Slice(rec.Items, func(i, j int) bool { return rec.Items[i].SKU < rec.Items[j].SKU })

	svc.st.shipments[s.OrderNumber] = append(svc.st.shipments[s.OrderNumber], rec)
	if s.ShipmentID != "" {
		svc.st.seenShipments[shipmentKey{s.OrderNumber, s.ShipmentID}] = struct{}{}
	}
	o.Status = svc.deriveStatusLocked(o)
	if err := svc.saved(); err != nil {
		return nil, err
	}
	return &ShipmentResult{Shipment: rec, Status: o.Status}, nil
}

// ShipmentResult 是发货确认结果。
type ShipmentResult struct {
	Shipment *Shipment
	Status   OrderStatus
}

// ---------- 取消 ----------

// CancelOrder 取消订单。
//
// 只释放“尚未拣货”的库存占用（含各版方案中仍开放的分配行）；已经拣出的数量
// 是实物履约事实，不回滚库存，也不允许再发货。取消是终态：重复取消幂等返回；
// 已全部发货的订单不能取消。取消、缺货重配、发货在同一把锁下串行，
// 库存占用、订单状态与发货记录始终一致。
func (svc *Service) CancelOrder(orderNo OrderNumber) error {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	o, err := svc.st.getOrder(orderNo)
	if err != nil {
		return err
	}
	if o.Status == OrderCancelled {
		return nil // 幂等
	}
	if o.Status == OrderShipped {
		return ErrOrderTerminal
	}

	for _, p := range svc.st.allPlans(orderNo) {
		for _, a := range p.Allocations {
			if a.Open() {
				svc.adjustReserved(a.WarehouseID, a.SKU, -(a.Quantity - a.PickedQuantity))
				a.Status = AllocCancelled
			}
		}
		// 仍挂着开放分配的活跃方案整体标记取消；已完成/已替代的历史方案保留原状态。
		if p.Status == PlanActive {
			p.Status = PlanCancelled
		}
	}
	o.Status = OrderCancelled
	return svc.saved()
}
