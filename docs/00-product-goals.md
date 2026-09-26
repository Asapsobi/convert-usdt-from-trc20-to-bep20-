# Product goals — read this first

_The product owner's own statement of what this system must do, confirmed
26 Sep 2026. It is the source of truth for this project: where any other
document in this repository disagrees with it, this document wins._

_The product is **public**: anyone can use it to convert USDT between TRC-20
and BEP-20 (decision recorded in
[`01-strategy/model-d-model-f-product-separation.md`](01-strategy/model-d-model-f-product-separation.md),
update of 26 Sep 2026)._

---

The main objective of this project is to build a system that can handle the
following end-to-end flow.

## 1. User Conversion Request

The user first selects:

* The asset they want to convert.
* The asset they want to receive.

At the current stage, the only supported asset is USDT, with support for the
following networks:

* BEP-20
* TRC-20

Once the user specifies the conversion, the system provides the user with a
wallet address to which they must send their funds.

## 2. Exchange Rate, Vendor Fee, and Our Profit

At this stage, based on the amount the user wants to convert, the system checks
the selected vendor/platform to determine the applicable commission or
conversion fee.

The calculation should then be:

**User Amount → Vendor Fee + Our Profit → Final Payout Amount**

Our profit margin is configured manually by an administrator. The system
calculates the applicable vendor fee and our profit against the user's
transaction amount and displays the final amount the user will receive.

The user should clearly see the final payout amount before proceeding with the
transaction.

## 3. Wallet Management

The deposit wallet addresses used by the system must be limited and centrally
managed.

We need to maintain a clear record of:

* Which wallets currently exist in the system.
* Which network each wallet belongs to.
* Which wallets are currently active.
* Which wallets are available for receiving deposits.

Limiting the number of wallets is important because it makes the operational
process of sweeping funds from deposit wallets to treasury wallets
significantly easier and less expensive.

This is particularly important for the TRON network, where transaction resource
requirements can create additional operational costs.

## 4. BEP-20 Wallet Requirements

For BEP-20 deposit wallets, the system must check whether the wallet has
sufficient BNB to cover the required transaction/network fees.

There are two possible scenarios:

**Scenario A — Sufficient BNB**

If the BEP-20 wallet contains enough BNB, the system can continue processing
the transaction normally.

**Scenario B — Insufficient BNB**

If the wallet does not have enough BNB, the system must fund the wallet before
continuing.

In this case:

1. The system identifies a suitable treasury wallet.
2. The required amount of BNB is transferred from the treasury wallet to the
   user's deposit wallet.
3. Once the user's USDT deposit and the required BNB balance are available, the
   transaction can proceed.

## 5. TRC-20 Wallet Requirements

TRC-20 wallets follow a different process because transactions on TRON require
sufficient Energy and Bandwidth.

As with BEP-20 wallets, the number of TRC-20 deposit wallets should be limited
to simplify the sweeping process and reduce operational costs.

Before processing a transaction, the system must check whether the specific
wallet has enough:

* Energy
* Bandwidth

**Scenario A — Sufficient Resources**

If the wallet has sufficient Energy and Bandwidth, the transaction can proceed
normally.

**Scenario B — Insufficient Resources**

If the wallet does not have sufficient resources, the system should use an
external resource provider/platform, such as CatFee or an equivalent service,
to reserve or rent the required Energy and Bandwidth for that specific wallet.

Before renting the resources, the system must determine the exact amount of
Energy required for the transaction.

For example, the system should determine whether the transaction requires
approximately:

* 132,000 Energy
* 65,000 Energy
* Or another appropriate amount based on the actual transaction requirements.

Only the required amount of Energy should be rented.

Once the user has completed the deposit, the system rents the required
Energy/Bandwidth for the wallet. After the required resources are available,
the transaction can proceed.

## 6. Conversion Through Fixed-Float or Alternative Vendors

After the user's deposit has been successfully received and all required
network resources are available, the system proceeds to an external
exchange/conversion platform such as FixedFloat, or another configured
alternative vendor.

The process is:

1. The user deposits USDT into our deposit wallet.
2. The system calculates our profit and the applicable vendor fees.
3. The amount remaining after our profit is deducted is sent to FixedFloat or
   the selected alternative vendor.
4. The user's requested payout/destination wallet is provided to the vendor as
   the destination address.
5. The vendor performs the conversion and sends the resulting payout asset to
   the user's destination wallet.
6. The transaction is marked as completed once the required confirmation
   conditions are met.

## 7. Treasury and Fund Sweeping

Our profit remains temporarily in the deposit wallets where users have made
their deposits.

These accumulated funds should periodically be swept from the deposit wallets
into one of the designated treasury wallets.

The system should therefore support an operational process for:

* Tracking balances across all deposit wallets.
* Identifying available balances that can be swept.
* Transferring accumulated funds to treasury wallets.
* Keeping the number of deposit wallets limited to reduce sweeping costs.

This is especially important for TRON because the cost and resource
requirements associated with moving funds between multiple wallets can become
significant.

## 8. Vendor Management and Selection

For each required service, the system should support multiple vendors where
applicable.

For example, there may be multiple vendors for:

* Asset conversion.
* Energy/Bandwidth rental.
* Liquidity.
* Other required infrastructure or transaction services.

Each vendor should have clearly defined configuration and pricing information.

When multiple vendors are available, the system should select the vendor based
on the configured business logic, such as:

* Lower transaction cost.
* Higher revenue/profit margin.
* Better exchange rate.
* Availability.
* Other operational criteria defined by the administrator.

The system should automatically calculate which available vendor provides the
most favorable configured outcome.

### Vendor Failover

If a vendor becomes unavailable, fails to respond, or cannot process the
transaction, the system should temporarily remove that vendor from the
available vendor pool.

The system should then recalculate the transaction using the remaining
available vendors.

Once the unavailable vendor becomes operational again, it can be returned to
the active vendor pool.

## 9. Key Operational Principles

The overall system should be designed around the following principles:

1. **Limited number of deposit wallets** — Keep the number of wallets under
   management as low as practical to simplify sweeping and reduce network
   costs.
2. **Automatic wallet resource checks** — Before processing a transaction,
   verify that the relevant wallet has sufficient BNB, Energy, or Bandwidth.
3. **Automatic resource provisioning** — If a wallet lacks the required network
   resources, the system should automatically fund or rent the required
   resources before continuing.
4. **Configurable profit margin** — Our profit margin should be configurable by
   an administrator.
5. **Multiple vendor support** — The system should support multiple vendors for
   each required service.
6. **Cost/revenue optimization** — Where multiple vendors are available, the
   system should select the configured vendor that provides the most favorable
   cost/revenue outcome.
7. **Automatic vendor failover** — If a vendor is unavailable, the system
   should temporarily disable it and continue processing using the remaining
   available vendors.
8. **Centralized treasury management** — Funds accumulated in deposit wallets
   should periodically be consolidated into designated treasury wallets.
9. **Full transaction tracking** — Every transaction should maintain a clear
   record of the user deposit, wallet used, network, vendor selected, fees,
   profit, payout amount, resource costs, and final transaction status.
