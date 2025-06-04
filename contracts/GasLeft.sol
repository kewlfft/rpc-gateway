// SPDX-License-Identifier: UNLICENSED
pragma solidity 0.8.30;

contract GasLeft {
    fallback() external payable {
        if (msg.sig == 0x22222222) {
            if (msg.value != 0) revert();
            uint256 g;
            assembly { g := gas() }
            assembly { return(add(0x00, 0x20), 0x20) }
        } else {
            revert();
        }
    }
}
