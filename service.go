package goorderfulfillment

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// FulfillmentService 是订单跨仓拆分履约的内存领域服务，并发安全。
//
// 所有公开方法在同一把互斥锁上串行执行，因此：
//   - 并发确认订单不会超卖（库存检查与占用是同一个临界区）；
//   - 取消、缺货重配、发货确认同时发生时，订单状态、库存占用与发货记录
//     永远作为一个整体保持一致。
//
// 每版拆分方案（PlanVersion）及其全部明细都完整保留在 orders/<order>.versions 中。
type FulfillmentService struct {
	mu sync.Mutex

	// inventories 键为 warehouseID+"\x00"+sku。
	inventories map[string]*Inventory
	// orders 以外部订单号为键。
	orders map[string]*Order
	// allocations 以分配 ID 为键，指向 versions 中的同一对象。
	allocations map[string]*Allocation
	// shipments 按订单保存发货记录。
	shipments map[string][]ShipmentRecord

	now func() time.Time
	// shipmentSeq 用于生成发货记录 ID（时间戳不可用时也保证唯一）。
	shipmentSeq int
}

// NewFulfillmentService 创建一个空的履约服务。
func NewFulfillmentService() *FulfillmentService {
	return &FulfillmentService{
		inventories: make(map[string]*Inventory),
		orders:      make(map[string]*Order),
		allocations: make(map[string]*Allocation),
		shipments:   make(map[string][]ShipmentRecord),
		now:         time.Now,
	}
}

func invKey(warehouseID, sku string) string { return warehouseID + "\x00" + sku }

// RegisterInventory 向仓库登记（追加）实物库存。
func (s *FulfillmentService) RegisterInventory(req RegisterInventoryRequest) (InventorySnapshot, error) {
	if req.WarehouseID == "" || req.SKU == "" || req.Quantity <= 0 {
		return InventorySnapshot{}, fmt.Errorf("%w: warehouse id, sku and positive quantity are required", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	inv := s.inventories[invKey(req.WarehouseID, req.SKU)]
	if inv == nil {
		inv = &Inventory{WarehouseID: req.WarehouseID, SKU: req.SKU}
		s.inventories[invKey(req.WarehouseID, req.SKU)] = inv
	}
	inv.OnHand += req.Quantity
	inv.Available += req.Quantity
	return snapshotOf(inv), nil
}

// GetInventory 查询某仓某 SKU 的库存台账；从未登记时返回零值快照。
func (s *FulfillmentService) GetInventory(warehouseID, sku string) InventorySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inv := s.inventories[invKey(warehouseID, sku)]; inv != nil {
		return snapshotOf(inv)
	}
	return InventorySnapshot{WarehouseID: warehouseID, SKU: sku}
}

// ConfirmOrder 确认订单：按各仓可用库存为全部订单行生成跨仓拆分方案并原子占用库存。
//
//   - 任意一行无法被全部仓库完整满足时整体拒绝，不会留下任何已占用库存；
//   - 相同外部订单号 + 相同内容重复确认：幂等返回原方案；
//   - 相同外部订单号但 SKU 或数量变化：返回 ErrOrderConflict。
func (s *FulfillmentService) ConfirmOrder(req ConfirmOrderRequest) (*Confirmation, error) {
	lines, fingerprint, err := normalizeLines(req.Lines)
	if err != nil {
		return nil, err
	}
	if req.ExternalOrderID == "" {
		return nil, fmt.Errorf("%w: external order id is required", ErrInvalidArgument)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if order := s.orders[req.ExternalOrderID]; order != nil {
		if order.ContentFingerprint != fingerprint {
			return nil, fmt.Errorf("%w: order %s", ErrOrderConflict, req.ExternalOrderID)
		}
		// 幂等：返回既有方案。订单已取消/已发完时没有活跃版本，返回最后一版。
		versions := orderVersions(order)
		latest := versions[len(versions)-1]
		version := order.ActiveVersion
		if version == 0 {
			version = latest.Version
		}
		return &Confirmation{
			OrderID:     order.ID,
			Version:     version,
			Reconfirmed: true,
			Allocations: cloneAllocations(latest.Allocations),
			Status:      order.Status,
		}, nil
	}

	plan, err := s.buildPlan(req.ExternalOrderID, 1, lines)
	if err != nil {
		return nil, err // 库存不足：尚未创建订单，也未占用任何库存
	}

	now := s.now()
	order := &Order{
		ID:                 req.ExternalOrderID,
		Lines:              lines,
		lineIndex:          indexLines(lines),
		ContentFingerprint: fingerprint,
		Status:             StatusPending,
		ActiveVersion:      1,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	s.orders[order.ID] = order
	s.applyPlan(order, plan, "confirm")

	return &Confirmation{
		OrderID:     order.ID,
		Version:     1,
		Reconfirmed: false,
		Allocations: cloneAllocations(plan),
		Status:      order.Status,
	}, nil
}

// PickingReceipt 处理一批仓库拣货回执。
//
//   - 成功拣出的数量立即落账（Reserved→Picked），即使后续行短缺重配也保留；
//   - 短缺数量核销（Reserved→Lost），该分配关闭；同一临界区内自动为剩余缺口
//     生成下一版跨仓方案；库存仍不足时保留缺口，可在补货后调用 Reallocate 重试；
//   - 迟到回执（分配已关闭/取消/被新版方案取代）整体拒绝，不会覆盖新分配，
//     也不会让同一商品被重复发出。
func (s *FulfillmentService) PickingReceipt(req PickingReceiptRequest) (*PickingReceiptResult, error) {
	if len(req.Items) == 0 {
		return nil, fmt.Errorf("%w: empty receipt", ErrInvalidArgument)
	}

	// 第一阶段：在不修改任何状态的前提下校验整批回执，保证原子性。
	type validated struct {
		alloc  *Allocation
		inv    *Inventory
		picked int
		short  int
	}
	checked := make([]validated, 0, len(req.Items))
	seen := make(map[string]bool)

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, item := range req.Items {
		if item.AllocationID == "" {
			return nil, fmt.Errorf("%w: allocation id is required", ErrInvalidArgument)
		}
		if item.PickedQty < 0 || item.ShortageQty < 0 || (item.PickedQty == 0 && item.ShortageQty == 0) {
			return nil, fmt.Errorf("%w: picked/shortage quantities must be non-negative and not both zero", ErrInvalidArgument)
		}
		if seen[item.AllocationID] {
			return nil, fmt.Errorf("%w: duplicate allocation %s in one receipt batch", ErrInvalidArgument, item.AllocationID)
		}
		seen[item.AllocationID] = true

		alloc := s.allocations[item.AllocationID]
		if alloc == nil {
			return nil, fmt.Errorf("%w: %s", ErrAllocationNotFound, item.AllocationID)
		}
		if alloc.Status != AllocActive {
			// 已拣齐、已因短缺关闭、已取消：任何迟到/重复回执一律拒绝。
			return nil, fmt.Errorf("%w: allocation %s is %s", ErrStaleAllocation, alloc.ID, alloc.Status)
		}
		if item.PickedQty+item.ShortageQty > alloc.outstanding() {
			return nil, fmt.Errorf("%w: allocation %s outstanding=%d got picked=%d shortage=%d",
				ErrReceiptOverflow, alloc.ID, alloc.outstanding(), item.PickedQty, item.ShortageQty)
		}
		inv := s.inventories[invKey(alloc.WarehouseID, alloc.LineSKU)]
		checked = append(checked, validated{alloc: alloc, inv: inv, picked: item.PickedQty, short: item.ShortageQty})
	}

	// 第二阶段：逐笔落账。
	result := &PickingReceiptResult{}
	now := s.now()
	shortageOrders := make(map[string]bool)

	for _, v := range checked {
		alloc, inv := v.alloc, v.inv
		alloc.PickedQty += v.picked
		alloc.ShortageQty += v.short
		alloc.UpdatedAt = now

		inv.Reserved -= v.picked + v.short
		inv.Picked += v.picked
		inv.Lost += v.short

		if v.short > 0 {
			shortageOrders[alloc.OrderID] = true
		}
		if alloc.outstanding() == 0 {
			if alloc.ShortageQty > 0 {
				alloc.Status = AllocShortageClosed
			} else {
				alloc.Status = AllocPickedComplete
			}
		}
		result.Updated = append(result.Updated, viewOf(alloc))
	}

	// 为每个出现短缺的订单尝试重配；失败（仍缺货）不回滚已拣数量，
	// 缺口保留待补货后 Reallocate 重试。
	orderIDs := make([]string, 0, len(shortageOrders))
	for id := range shortageOrders {
		orderIDs = append(orderIDs, id)
	}
	sort.Strings(orderIDs)
	for _, id := range orderIDs {
		ver, allocs, err := s.reallocateLocked(s.orders[id], "reallocate after shortage")
		if result.ReallocatedVersion == 0 {
			result.ReallocatedVersion = ver
			result.Reallocated = allocs
			result.ReallocateError = err
		}
	}
	for _, v := range checked {
		order := s.orders[v.alloc.OrderID]
		s.refreshStatus(order, now)
		order.UpdatedAt = now
	}
	return result, nil
}

// ReceiptPick 便捷方法：为某条分配提交一份“全部成功”的拣货回执。
func (s *FulfillmentService) ReceiptPick(allocationID string, qty int) (*PickingReceiptResult, error) {
	return s.PickingReceipt(PickingReceiptRequest{
		Items: []PickingReceiptItem{{AllocationID: allocationID, PickedQty: qty}},
	})
}

// Reallocate 为订单当前所有“未被活跃分配覆盖”的缺口显式生成下一版跨仓方案。
// 典型用途：拣货短缺自动重配时因库存不足失败，补货后手动重试。
// 没有缺口时返回 ErrNothingToReallocate。
func (s *FulfillmentService) Reallocate(req ReallocateRequest) (*ReallocateResult, error) {
	if req.OrderID == "" {
		return nil, fmt.Errorf("%w: order id is required", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	order := s.orders[req.OrderID]
	if order == nil {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, req.OrderID)
	}
	if order.Status == StatusCancelled {
		return nil, fmt.Errorf("%w: order %s is cancelled", ErrInvalidArgument, order.ID)
	}
	gaps := s.uncoveredGaps(order)
	if totalGap(gaps) == 0 {
		return nil, ErrNothingToReallocate
	}
	ver, allocs, err := s.reallocateLocked(order, "manual reallocate")
	if err != nil {
		return nil, err
	}
	return &ReallocateResult{Version: ver, Allocations: allocs}, nil
}

// reallocateLocked 调用方必须持有 mu。计算缺口、构造并应用新版本。
func (s *FulfillmentService) reallocateLocked(order *Order, note string) (int, []*Allocation, error) {
	gaps := s.uncoveredGaps(order)
	if totalGap(gaps) == 0 {
		return 0, nil, ErrNothingToReallocate
	}
	version := order.ActiveVersion + 1
	plan, err := s.buildPlan(order.ID, version, gaps)
	if err != nil {
		return 0, nil, err
	}
	s.applyPlan(order, plan, note)
	return version, cloneAllocations(plan), nil
}

// uncoveredGaps 返回每个 SKU 尚未被覆盖的需求数量。
// 覆盖口径：已成功拣出的数量 + 仍活跃（未关闭）分配的待拣量。
// 注意：已核销的短缺数量不计入覆盖——它正是需要新版本承接的缺口；
// 短缺关闭的分配不再贡献覆盖，其短缺部分天然成为缺口。
func (s *FulfillmentService) uncoveredGaps(order *Order) []OrderLine {
	covered := make(map[string]int) // sku -> 已拣 + 活跃分配 outstanding
	for _, v := range orderVersions(order) {
		for _, a := range v.Allocations {
			covered[a.LineSKU] += a.PickedQty
			if a.Status == AllocActive {
				covered[a.LineSKU] += a.outstanding()
			}
		}
	}
	var gaps []OrderLine
	for _, line := range order.Lines {
		if gap := line.Quantity - covered[line.SKU]; gap > 0 {
			gaps = append(gaps, OrderLine{SKU: line.SKU, Quantity: gap})
		}
	}
	return gaps
}

// Ship 仓库确认发货。只能发该仓“已成功拣出但尚未发出”的数量，
// 超发被拒绝，因此同一件商品不可能被两个仓重复发出。
func (s *FulfillmentService) Ship(req ShipmentRequest) (*ShipmentRecord, error) {
	if req.AllocationID == "" {
		return nil, fmt.Errorf("%w: allocation id is required", ErrInvalidArgument)
	}
	if req.Quantity <= 0 {
		return nil, fmt.Errorf("%w: quantity must be positive", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	alloc := s.allocations[req.AllocationID]
	if alloc == nil {
		return nil, fmt.Errorf("%w: %s", ErrAllocationNotFound, req.AllocationID)
	}
	order := s.orders[alloc.OrderID]
	if order.Status == StatusShipped {
		return nil, ErrAlreadyShipped
	}
	if req.Quantity > alloc.PickedQty-alloc.ShippedQty {
		return nil, fmt.Errorf("%w: allocation %s picked=%d shipped=%d requested=%d",
			ErrShipExceedsPicked, alloc.ID, alloc.PickedQty, alloc.ShippedQty, req.Quantity)
	}

	now := s.now()
	s.shipmentSeq++
	record := ShipmentRecord{
		ID:           fmt.Sprintf("%s#ship-%d", order.ID, s.shipmentSeq),
		OrderID:      order.ID,
		AllocationID: alloc.ID,
		LineSKU:      alloc.LineSKU,
		WarehouseID:  alloc.WarehouseID,
		Quantity:     req.Quantity,
		CreatedAt:    now,
	}
	alloc.ShippedQty += req.Quantity
	alloc.UpdatedAt = now

	inv := s.inventories[invKey(alloc.WarehouseID, alloc.LineSKU)]
	inv.Picked -= req.Quantity
	inv.Shipped += req.Quantity

	s.shipments[order.ID] = append(s.shipments[order.ID], record)
	s.refreshStatus(order, now)
	order.UpdatedAt = now

	rec := record
	return &rec, nil
}

// Cancel 取消订单。仅释放尚未拣货（活跃分配剩余待拣）的库存；
// 已成功拣出的数量保留，仓库仍可对其发货。
// 取消后订单不再允许重配，任何针对已释放分配的迟到拣货回执都会被拒绝。
func (s *FulfillmentService) Cancel(req CancelRequest) (*FulfillmentDetail, error) {
	if req.OrderID == "" {
		return nil, fmt.Errorf("%w: order id is required", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	order := s.orders[req.OrderID]
	if order == nil {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, req.OrderID)
	}
	if order.Status != StatusCancelled && order.Status != StatusShipped {
		now := s.now()
		for _, v := range orderVersions(order) {
			for _, a := range v.Allocations {
				if a.Status != AllocActive {
					continue
				}
				// 释放尚未拣货的部分；已拣出的保留为 Picked。
				release := a.outstanding()
				if release > 0 {
					inv := s.inventories[invKey(a.WarehouseID, a.LineSKU)]
					inv.Reserved -= release
					inv.Available += release
				}
				a.Status = AllocCancelled
				a.UpdatedAt = now
			}
		}
		order.Status = StatusCancelled
		order.ActiveVersion = 0
		s.refreshStatus(order, now) // 已拣部分若全部已发，则归一为 shipped
		order.UpdatedAt = now
	}
	return s.detailLocked(order), nil
}

// GetFulfillmentDetail 查询订单完整履约明细（含每版方案与发货记录的深拷贝）。
func (s *FulfillmentService) GetFulfillmentDetail(orderID string) (*FulfillmentDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order := s.orders[orderID]
	if order == nil {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, orderID)
	}
	return s.detailLocked(order), nil
}

// ---------- 内部辅助（调用方持锁） ----------

// buildPlan 按各仓可用库存为全部 lines 构造完整拆分方案。
// 纯计算：不修改库存、不写入状态。任一 SKU 不足即返回 ErrInsufficientInventory，
// 因此调用方之后 applyPlan 时不可能出现半占用。
func (s *FulfillmentService) buildPlan(orderID string, version int, lines []OrderLine) ([]*Allocation, error) {
	now := s.now()
	var plan []*Allocation

	for _, line := range lines {
		need := line.Quantity

		// 可用库存降序，数量相同按仓库 ID 升序，保证方案确定可复现。
		warehouses := make([]*Inventory, 0)
		for _, inv := range s.inventories {
			if inv.SKU == line.SKU && inv.Available > 0 {
				warehouses = append(warehouses, inv)
			}
		}
		sort.Slice(warehouses, func(i, j int) bool {
			if warehouses[i].Available != warehouses[j].Available {
				return warehouses[i].Available > warehouses[j].Available
			}
			return warehouses[i].WarehouseID < warehouses[j].WarehouseID
		})

		for _, inv := range warehouses {
			if need == 0 {
				break
			}
			take := need
			if take > inv.Available {
				take = inv.Available
			}
			plan = append(plan, &Allocation{
				ID:          fmt.Sprintf("%s#v%d-%s-%s", orderID, version, line.SKU, inv.WarehouseID),
				OrderID:     orderID,
				Version:     version,
				LineSKU:     line.SKU,
				WarehouseID: inv.WarehouseID,
				Quantity:    take,
				Status:      AllocActive,
				CreatedAt:   now,
				UpdatedAt:   now,
			})
			need -= take
		}
		if need > 0 {
			return nil, fmt.Errorf("%w: sku %s short by %d", ErrInsufficientInventory, line.SKU, need)
		}
	}
	return plan, nil
}

// applyPlan 落地方案：占用各仓可用库存、写入新版本并登记分配索引。
func (s *FulfillmentService) applyPlan(order *Order, plan []*Allocation, note string) {
	now := s.now()
	version := &PlanVersion{
		Version:     plan[0].Version,
		OrderID:     order.ID,
		Allocations: plan,
		CreatedAt:   now,
		Note:        note,
	}
	versions := orderVersions(order)
	order.setVersions(append(versions, version))
	order.ActiveVersion = version.Version

	for _, a := range plan {
		inv := s.inventories[invKey(a.WarehouseID, a.LineSKU)]
		inv.Available -= a.Quantity
		inv.Reserved += a.Quantity
		s.allocations[a.ID] = a
	}
	order.UpdatedAt = now
}

// refreshStatus 依据逐行发货情况重算订单状态。
// cancelled 是显式终态；部分取消（仍有已拣未发）保持 cancelled，全部发出后亦归一为 shipped。
func (s *FulfillmentService) refreshStatus(order *Order, now time.Time) {
	shipped := make(map[string]int)
	pickedOrActive := false
	for _, v := range orderVersions(order) {
		for _, a := range v.Allocations {
			shipped[a.LineSKU] += a.ShippedQty
			if a.PickedQty-a.ShippedQty > 0 || a.Status == AllocActive {
				pickedOrActive = true
			}
		}
	}
	allShipped := true
	for _, line := range order.Lines {
		if shipped[line.SKU] < line.Quantity {
			allShipped = false
			break
		}
	}
	switch {
	case allShipped:
		order.Status = StatusShipped
		order.ActiveVersion = 0
	case order.Status == StatusCancelled:
		// 保持取消态；已拣部分允许继续发货。
	case pickedOrActive:
		order.Status = StatusInFulfillment
	case totalGap(s.uncoveredGaps(order)) > 0:
		// 短缺关闭后重配失败、缺口悬空：仍处履约中，补货后可 Reallocate。
		order.Status = StatusInFulfillment
	default:
		order.Status = StatusPending
	}
}

func (s *FulfillmentService) detailLocked(order *Order) *FulfillmentDetail {
	summary := make(map[string]*LineFulfillment)
	for _, line := range order.Lines {
		summary[line.SKU] = &LineFulfillment{SKU: line.SKU, Demanded: line.Quantity}
	}
	for _, v := range cloneVersions(orderVersions(order)) {
		for _, a := range v.Allocations {
			l := summary[a.LineSKU]
			l.Picked += a.PickedQty
			l.Shipped += a.ShippedQty
			l.Lost += a.ShortageQty
		}
	}
	for _, gap := range s.uncoveredGaps(order) {
		summary[gap.SKU].Outstanding = gap.Quantity
	}
	lines := make([]LineFulfillment, 0, len(order.Lines))
	for _, line := range order.Lines {
		lines = append(lines, *summary[line.SKU])
	}

	shipments := append([]ShipmentRecord(nil), s.shipments[order.ID]...)

	return &FulfillmentDetail{
		OrderID:       order.ID,
		Status:        order.Status,
		ActiveVersion: order.ActiveVersion,
		Lines:         append([]OrderLine(nil), order.Lines...),
		Versions:      cloneVersions(orderVersions(order)),
		Shipments:     shipments,
		LineSummary:   lines,
		CreatedAt:     order.CreatedAt,
		UpdatedAt:     order.UpdatedAt,
	}
}

// ---------- 纯函数辅助 ----------

func normalizeLines(in []OrderLine) ([]OrderLine, string, error) {
	if len(in) == 0 {
		return nil, "", fmt.Errorf("%w: at least one order line is required", ErrInvalidArgument)
	}
	merged := make(map[string]int)
	skus := make([]string, 0, len(in))
	for _, l := range in {
		if l.SKU == "" || l.Quantity <= 0 {
			return nil, "", fmt.Errorf("%w: each line needs a sku and positive quantity", ErrInvalidArgument)
		}
		if _, ok := merged[l.SKU]; !ok {
			skus = append(skus, l.SKU)
		}
		merged[l.SKU] += l.Quantity
	}
	sort.Strings(skus)
	lines := make([]OrderLine, 0, len(skus))
	var b strings.Builder
	for _, sku := range skus {
		lines = append(lines, OrderLine{SKU: sku, Quantity: merged[sku]})
		fmt.Fprintf(&b, "%s=%d;", sku, merged[sku])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return lines, hex.EncodeToString(sum[:]), nil
}

func indexLines(lines []OrderLine) map[string]int {
	idx := make(map[string]int, len(lines))
	for i, l := range lines {
		idx[l.SKU] = i
	}
	return idx
}

func totalGap(lines []OrderLine) int {
	n := 0
	for _, l := range lines {
		n += l.Quantity
	}
	return n
}

func snapshotOf(inv *Inventory) InventorySnapshot {
	return InventorySnapshot{
		WarehouseID: inv.WarehouseID,
		SKU:         inv.SKU,
		OnHand:      inv.OnHand,
		Available:   inv.Available,
		Reserved:    inv.Reserved,
		Picked:      inv.Picked,
		Shipped:     inv.Shipped,
		Lost:        inv.Lost,
	}
}

func viewOf(a *Allocation) AllocationView {
	return AllocationView{
		ID:          a.ID,
		Version:     a.Version,
		LineSKU:     a.LineSKU,
		WarehouseID: a.WarehouseID,
		Quantity:    a.Quantity,
		PickedQty:   a.PickedQty,
		ShippedQty:  a.ShippedQty,
		ShortageQty: a.ShortageQty,
		Status:      a.Status,
	}
}

func cloneAllocations(in []*Allocation) []*Allocation {
	out := make([]*Allocation, len(in))
	for i, a := range in {
		cp := *a
		out[i] = &cp
	}
	return out
}

func cloneVersions(in []*PlanVersion) []*PlanVersion {
	out := make([]*PlanVersion, len(in))
	for i, v := range in {
		cp := *v
		cp.Allocations = cloneAllocations(v.Allocations)
		out[i] = &cp
	}
	return out
}
