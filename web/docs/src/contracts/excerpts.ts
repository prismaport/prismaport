// Every code excerpt the Smart contracts page shows. tests/contracts-excerpts.test.ts checks that
// each one taken from contracts/src still appears in its file (whitespace aside; a `// ...` line
// marks an elision), so an excerpt cannot drift from the contracts.

type ContractFile = 'Factory.sol' | 'ForwarderImpl.sol' | 'Gateway.sol' | 'FeeLib.sol' | 'SplitLib.sol';

/** USDC's side of EIP-3009: the token standard's rules, paraphrased, not code from this repository. */
export const EIP3009 = 'USDC (EIP-3009)';

export interface Excerpt {
  source: ContractFile | typeof EIP3009;
  code: string;
}

const c = (source: ContractFile, code: string): Excerpt => ({ source, code });

export const X = {
  factoryNoOwner: c(
    'Factory.sol',
    `/// @notice Deploys deposit-address clones of a single ForwarderImpl with CREATE2. No owner, no
///         admin: the address of a forwarder is a pure function of this factory, \`salt\` and \`Params\`.`,
  ),
  factoryPredict: c(
    'Factory.sol',
    `function predict(bytes32 salt, Params memory p) public view returns (address) {
    return
        LibClone.predictDeterministicAddress(implementation, encodeArgs(p), salt, address(this));
}`,
  ),
  factoryArgs: c(
    'Factory.sol',
    `args = abi.encodePacked(p.treasury, p.feeBps, p.fixedFeeMicros, uint8(n), uint8(nT));
for (uint256 i; i < n; ++i) {
    args = abi.encodePacked(args, p.recipients[i], p.bps[i]);
}
for (uint256 i; i < nT; ++i) {
    args = abi.encodePacked(args, p.tokens[i]);
}`,
  ),
  factoryEncodeChecks: c(
    'Factory.sol',
    `function encodeArgs(Params memory p) public pure returns (bytes memory args) {
    if (p.treasury == address(0)) revert ZeroAddress();
    FeeLib.checkParams(p.feeBps, p.fixedFeeMicros);
    SplitLib.check(p.recipients, p.bps);`,
  ),
  factoryDeployNotice: c(
    'Factory.sol',
    `/// @notice Deploys the forwarder for (\`salt\`, \`p\`). Idempotent: if it already exists (for
///         example because someone else deployed it first) the existing address is returned.`,
  ),
  factoryDeployAndFlush: c(
    'Factory.sol',
    `function deployAndFlush(bytes32 salt, Params memory p, address[] calldata tokens)
    external
    returns (address forwarder)
{
    forwarder = deploy(salt, p);
    for (uint256 i; i < tokens.length; ++i) {
        ForwarderImpl(forwarder).flush(tokens[i]);
    }
}`,
  ),
  feeCompute: c(
    'FeeLib.sol',
    `checkParams(feeBps, fixedFeeMicros);
// ...
fee = fixedFee(fixedFeeMicros, decimals) + pctFee(gross, feeBps);
if (fee >= gross) return (false, 0, 0);
settles = true;
net = gross - fee;`,
  ),
  feeDust: c('FeeLib.sol', 'if (fee >= gross) return (false, 0, 0);'),
  feeCaps: c(
    'FeeLib.sol',
    `uint16 internal constant MAX_FEE_BPS = 300;
uint64 internal constant MAX_FIXED_FEE_MICROS = 5_000_000;`,
  ),
  feeCheckParams: c(
    'FeeLib.sol',
    `function checkParams(uint16 feeBps, uint64 fixedFeeMicros) internal pure {
    if (feeBps > MAX_FEE_BPS) revert FeeBpsTooHigh(feeBps);
    if (fixedFeeMicros > MAX_FIXED_FEE_MICROS) revert FixedFeeTooHigh(fixedFeeMicros);
}`,
  ),
  splitLoop: c(
    'SplitLib.sol',
    `uint256 rest = net;
for (uint256 i = 1; i < n; ++i) {
    uint256 s = FeeLib.bpsOf(net, bps[i]);
    shares[i] = s;
    rest -= s;
}
shares[0] = rest;`,
  ),
  splitRemainder: c(
    'SplitLib.sol',
    `/// @dev Expects \`bps\` to have passed \`check\`. The first share takes the rounding remainder, so
///      it is at least its exact share and at most n - 1 base units above it.
// ...
shares[0] = rest;`,
  ),
  splitCheck: c(
    'SplitLib.sol',
    `if (n == 0 || n > MAX_RECIPIENTS) revert InvalidRecipientCount(n);
// ...
if (r == address(0)) revert ZeroRecipient(i);
for (uint256 j; j < i; ++j) {
    if (recipients[j] == r) revert DuplicateRecipient(i);
}
if (bps[i] == 0) revert ZeroShare(i);
sum += bps[i];
// ...
if (sum != BPS_TOTAL) revert InvalidShareSum(sum);`,
  ),
  forwarderNotice: c(
    'ForwarderImpl.sol',
    `///         Anyone may flush a bound token; funds can only go to \`treasury\` (fee) and the recipients
///         (net, split by SplitLib). Any other token, and native currency, stays at the address
///         unless the treasury forwards it with \`recover\`: there is no \`receive\`, so a native
///         send after deployment reverts.`,
  ),
  forwarderFlush: c(
    'ForwarderImpl.sol',
    `(settles, fee, net) =
    FeeLib.compute(balance, IERC20Decimals(token).decimals(), c.feeBps, c.fixedFeeMicros);
if (!settles) return (0, 0, 0);
gross = balance;

if (fee != 0) SafeTransferLib.safeTransfer(token, c.treasury, fee);
_payout(token, net, c.recipients, c.bps);
emit Settled(token, gross, fee, net, c.treasury);`,
  ),
  forwarderTokenCheck: c(
    'ForwarderImpl.sol',
    `// First, so an unlisted contract is never called (not even balanceOf or decimals) and the
// fee rule, which values one unit at US$1, only ever applies to the agreed stablecoins.
if (!TokenList.contains(c.tokens, token)) revert TokenNotAllowed(token);`,
  ),
  forwarderEvents: c(
    'ForwarderImpl.sol',
    `event Settled(address indexed token, uint256 gross, uint256 fee, uint256 net, address treasury);
/// @notice One per non-zero share.
event Payout(address indexed token, address indexed recipient, uint256 amount);`,
  ),
  gatewayNotice: c(
    'Gateway.sol',
    `/// @notice Signed-authorization payment path for EIP-3009 tokens. The payer signs a
///         ReceiveWithAuthorization whose nonce commits to the session, the treasury, the fee
///         parameters and the recipients with their shares, so none of them can be altered by
///         whoever submits it. The Gateway never keeps a balance: every settlement pays out exactly
///         what it pulled in.`,
  ),
  gatewayTreasury: c('Gateway.sol', 'address public immutable treasury;'),
  gatewayNonce: c(
    'Gateway.sol',
    `return keccak256(
    abi.encode(
        block.chainid,
        address(this),
        sessionSalt,
        treasury,
        feeBps,
        fixedFeeMicros,
        recipients,
        bps
    )
);`,
  ),
  gatewayChecks: c(
    'Gateway.sol',
    `SplitLib.check(a.recipients, a.bps);
for (uint256 i; i < a.recipients.length; ++i) {
    if (a.recipients[i] == address(this)) revert InvalidRecipient(i);
}
// ...
(settles, fee, net) =
    FeeLib.compute(a.value, IERC20Decimals(a.token).decimals(), a.feeBps, a.fixedFeeMicros);
if (!settles) revert DustPayment();`,
  ),
  gatewayDust: c('Gateway.sol', 'if (!settles) revert DustPayment();'),
  gatewayPullCall: c(
    'Gateway.sol',
    `bytes32 nonce = computeNonce(a.sessionSalt, a.feeBps, a.fixedFeeMicros, a.recipients, a.bps);
_pull(a, nonce);`,
  ),
  gatewayPull: c(
    'Gateway.sol',
    `uint256 before = IERC20Balance(a.token).balanceOf(address(this));
IERC3009(a.token)
    .receiveWithAuthorization(
        a.from, address(this), a.value, a.validAfter, a.validBefore, nonce, a.v, a.r, a.s
    );
uint256 received = IERC20Balance(a.token).balanceOf(address(this)) - before;
if (received != a.value) revert ReceivedAmountMismatch(a.value, received);`,
  ),
  gatewayBalanceCheck: c(
    'Gateway.sol',
    `uint256 received = IERC20Balance(a.token).balanceOf(address(this)) - before;
if (received != a.value) revert ReceivedAmountMismatch(a.value, received);`,
  ),
  gatewayPayout: c(
    'Gateway.sol',
    `if (fee != 0) SafeTransferLib.safeTransfer(a.token, treasury, fee);
uint256[] memory shares = SplitLib.split(net, a.bps);
for (uint256 i; i < shares.length; ++i) {
    if (shares[i] == 0) continue;
    SafeTransferLib.safeTransfer(a.token, a.recipients[i], shares[i]);
    emit Payout(a.token, a.recipients[i], shares[i]);
}
emit PaymentReceived(nonce, a.token, a.from, a.value, fee, net);`,
  ),
  usdcTypedData: {
    source: EIP3009,
    code: `// The message the payer signs (EIP-712 typed data)
ReceiveWithAuthorization(address from, address to, uint256 value,
    uint256 validAfter, uint256 validBefore, bytes32 nonce)`,
  },
  usdcPayee: {
    source: EIP3009,
    code: `// receiveWithAuthorization: the caller must be the payee
require(to == msg.sender);`,
  },
  usdcOnce: {
    source: EIP3009,
    code: `// Every authorization nonce can be used once
require(!authorizationState[from][nonce]);
authorizationState[from][nonce] = true;`,
  },
} satisfies Record<string, Excerpt>;

export const ALL_EXCERPTS: Excerpt[] = Object.values(X);
